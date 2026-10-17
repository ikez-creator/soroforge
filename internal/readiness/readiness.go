// Package readiness answers whether the dependencies SoroForge needs in order
// to do its job are usable.
//
// It exists for orchestrators. Liveness (`/health`) says the process is up;
// readiness says it can actually record deploys — the database answers and
// every configured network's RPC responds. An instance whose database is down
// should be taken out of rotation, not restarted, and this is the signal that
// says so.
//
// The checks are read-only by construction: they call ListContracts on the
// store and LatestLedger on each network, so a probe can never observe or
// alter deployment state. Calls are bounded by a short timeout and the result
// is cached for a brief TTL, because a probe typically runs every few seconds
// and must not hammer an RPC endpoint.
package readiness

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
)

// Store is the database dependency, reduced to the one read that proves the
// connection, credentials, and schema all work. store.Store satisfies it.
//
// Declaring the interface here rather than importing a concrete store keeps
// this package (and therefore the HTTP API) independent of Postgres.
type Store interface {
	ListContracts(ctx context.Context, network string) ([]store.Contract, error)
}

// ClientFactory creates a Stellar client for a named network. It matches
// deploy.ClientFactory, so the API can reuse the same factory the deploy
// service uses.
type ClientFactory func(networkName string, network config.Network) (stellar.Client, error)

// DatabaseName is the dependency name reported for Postgres.
const DatabaseName = "database"

// RPCName is the dependency name reported for a network's RPC endpoint.
func RPCName(network string) string { return "rpc:" + network }

// Defaults for Options.Timeout and Options.TTL.
const (
	// DefaultTimeout bounds each dependency check. An orchestrator's probe
	// timeout is usually a few seconds; a check that takes longer than this
	// is as good as failed.
	DefaultTimeout = 3 * time.Second

	// DefaultTTL is how long a result is reused. Probes often run every few
	// seconds; without a cache, every replica's probe would hit the RPC.
	DefaultTTL = 5 * time.Second
)

// Options configures a Checker. Store, Config, and Clients are required.
type Options struct {
	// Store proves the database is reachable.
	Store Store

	// Config supplies the networks to check. Each one's RPC endpoint must
	// return a latest ledger.
	Config *config.Config

	// Clients creates a client per network. Required.
	Clients ClientFactory

	// Log defaults to slog.Default().
	Log *slog.Logger

	// Timeout bounds each dependency check. Defaults to DefaultTimeout.
	Timeout time.Duration

	// TTL is how long a result is reused. Defaults to DefaultTTL.
	TTL time.Duration

	// Now supplies the clock, so tests can exercise the cache without
	// sleeping. Defaults to time.Now.
	Now func() time.Time
}

// Dependency is one checked component.
type Dependency struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`

	// Error is why the dependency is not usable. It is empty when OK.
	Error string `json:"error,omitempty"`
}

// Status is the outcome of a readiness check. Ready is true only when every
// dependency is OK.
type Status struct {
	Ready        bool         `json:"ready"`
	Dependencies []Dependency `json:"dependencies"`
}

// Checker runs the dependency checks and caches their result.
type Checker struct {
	store    Store
	networks map[string]config.Network
	clients  ClientFactory
	log      *slog.Logger
	timeout  time.Duration
	ttl      time.Duration
	now      func() time.Time

	// mu guards the cache and serialises refreshes, so that a burst of probes
	// arriving after the TTL expires produces one check, not one per probe.
	mu        sync.Mutex
	cached    Status
	checkedAt time.Time
	checked   bool
}

// New builds a Checker.
func New(opts Options) (*Checker, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("readiness: store is required")
	}
	if opts.Config == nil {
		return nil, fmt.Errorf("readiness: config is required")
	}
	if opts.Clients == nil {
		return nil, fmt.Errorf("readiness: client factory is required")
	}

	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Checker{
		store:    opts.Store,
		networks: opts.Config.Networks,
		clients:  opts.Clients,
		log:      log,
		timeout:  timeout,
		ttl:      ttl,
		now:      now,
	}, nil
}

// Check returns the current readiness, re-running the probes at most once per
// TTL. The returned Status is a snapshot safe to serialise directly.
func (c *Checker) Check(ctx context.Context) Status {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.checked && c.now().Sub(c.checkedAt) < c.ttl {
		return c.cached
	}

	status := c.probe(ctx)
	c.cached = status
	c.checkedAt = c.now()
	c.checked = true

	for _, dep := range status.Dependencies {
		if !dep.OK {
			c.log.Debug("dependency is not ready", "name", dep.Name, "error", dep.Error)
		}
	}
	return status
}

// probe checks every dependency concurrently, each with its own timeout so a
// slow endpoint cannot starve the others. The database goes first in the
// result because it is the one most likely to fail and the most important to
// see.
func (c *Checker) probe(ctx context.Context) Status {
	names := make([]string, 0, len(c.networks))
	for name := range c.networks {
		names = append(names, name)
	}
	// Sorted for a stable result order, which makes the endpoint's output
	// diffable between probes.
	sort.Strings(names)

	deps := make([]Dependency, 1+len(names))
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		deps[0] = c.checkDatabase(ctx)
	}()

	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deps[i+1] = c.checkRPC(ctx, name, c.networks[name])
		}()
	}
	wg.Wait()

	status := Status{Ready: true, Dependencies: deps}
	for _, dep := range deps {
		if !dep.OK {
			status.Ready = false
			break
		}
	}
	return status
}

func (c *Checker) checkDatabase(ctx context.Context) Dependency {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// One query proves the connection, credentials, and schema. The result
	// itself is irrelevant, hence the throwaway network.
	if _, err := c.store.ListContracts(ctx, ""); err != nil {
		return Dependency{Name: DatabaseName, Error: err.Error()}
	}
	return Dependency{Name: DatabaseName, OK: true}
}

func (c *Checker) checkRPC(ctx context.Context, name string, network config.Network) Dependency {
	dep := Dependency{Name: RPCName(name)}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	client, err := c.clients(name, network)
	if err != nil {
		dep.Error = err.Error()
		return dep
	}

	ledger, err := client.LatestLedger(ctx)
	if err != nil {
		dep.Error = err.Error()
		return dep
	}
	if ledger == 0 {
		dep.Error = "RPC returned ledger 0"
		return dep
	}

	dep.OK = true
	return dep
}
