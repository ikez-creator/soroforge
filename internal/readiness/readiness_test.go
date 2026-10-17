package readiness

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func twoNetworks() *config.Config {
	return &config.Config{
		Networks: map[string]config.Network{
			"testnet": {RPCURL: "https://rpc.testnet.example", Passphrase: "Test SDF Network ; September 2015"},
			"mainnet": {RPCURL: "https://rpc.mainnet.example", Passphrase: "Public Global Stellar Network ; September 2015"},
		},
	}
}

// brokenStore fails every query, standing in for a database that is down.
type brokenStore struct{ err error }

func (b brokenStore) ListContracts(context.Context, string) ([]store.Contract, error) {
	return nil, b.err
}

// failingClient has a working interface except for LatestLedger.
type failingClient struct {
	stellar.Client
	err error
}

func (f failingClient) LatestLedger(context.Context) (uint32, error) { return 0, f.err }

// slowClient blocks until the check's context expires.
type slowClient struct{ stellar.Client }

func (slowClient) LatestLedger(ctx context.Context) (uint32, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func healthyFactory() ClientFactory {
	return func(string, config.Network) (stellar.Client, error) {
		return stellar.NewMockClient(), nil
	}
}

func TestCheckReportsHealthy(t *testing.T) {
	checker, err := New(Options{
		Store:   store.NewMemoryStore(),
		Config:  twoNetworks(),
		Clients: healthyFactory(),
	})
	require.NoError(t, err)

	status := checker.Check(context.Background())

	assert.True(t, status.Ready)
	require.Len(t, status.Dependencies, 3)

	// Database first, then networks sorted, so probe output is stable.
	assert.Equal(t, DatabaseName, status.Dependencies[0].Name)
	assert.Equal(t, RPCName("mainnet"), status.Dependencies[1].Name)
	assert.Equal(t, RPCName("testnet"), status.Dependencies[2].Name)
	for _, dep := range status.Dependencies {
		assert.True(t, dep.OK, "%s should be OK", dep.Name)
		assert.Empty(t, dep.Error)
	}
}

func TestCheckReportsDatabaseDown(t *testing.T) {
	checker, err := New(Options{
		Store:   brokenStore{err: errors.New("dial tcp 127.0.0.1:5432: connection refused")},
		Config:  twoNetworks(),
		Clients: healthyFactory(),
	})
	require.NoError(t, err)

	status := checker.Check(context.Background())

	assert.False(t, status.Ready)

	byName := dependenciesByName(status)
	assert.False(t, byName[DatabaseName].OK)
	assert.Contains(t, byName[DatabaseName].Error, "connection refused")
	// The RPCs are fine and still reported, so a 503 body names every
	// failing dependency rather than just the first.
	assert.True(t, byName[RPCName("testnet")].OK)
	assert.True(t, byName[RPCName("mainnet")].OK)
}

func TestCheckReportsOneRPCDown(t *testing.T) {
	checker, err := New(Options{
		Store:  store.NewMemoryStore(),
		Config: twoNetworks(),
		Clients: func(name string, _ config.Network) (stellar.Client, error) {
			if name == "mainnet" {
				return failingClient{err: errors.New("connection refused")}, nil
			}
			return stellar.NewMockClient(), nil
		},
	})
	require.NoError(t, err)

	status := checker.Check(context.Background())

	assert.False(t, status.Ready)

	byName := dependenciesByName(status)
	assert.True(t, byName[DatabaseName].OK)
	assert.True(t, byName[RPCName("testnet")].OK)
	assert.False(t, byName[RPCName("mainnet")].OK)
	assert.Contains(t, byName[RPCName("mainnet")].Error, "connection refused")
}

func TestCheckReportsClientFactoryFailure(t *testing.T) {
	checker, err := New(Options{
		Store:  store.NewMemoryStore(),
		Config: &config.Config{Networks: map[string]config.Network{"bad": {}}},
		Clients: func(string, config.Network) (stellar.Client, error) {
			return nil, errors.New("stellar: RPC URL is required")
		},
	})
	require.NoError(t, err)

	status := checker.Check(context.Background())

	assert.False(t, status.Ready)
	assert.Contains(t, dependenciesByName(status)[RPCName("bad")].Error, "RPC URL is required")
}

func TestCheckTreatsLedgerZeroAsFailure(t *testing.T) {
	// An RPC that answers "ledger 0" has not synced to the network.
	checker, err := New(Options{
		Store:  store.NewMemoryStore(),
		Config: &config.Config{Networks: map[string]config.Network{"testnet": {}}},
		Clients: func(string, config.Network) (stellar.Client, error) {
			mock := stellar.NewMockClient()
			mock.LedgerSeq = 0
			return mock, nil
		},
	})
	require.NoError(t, err)

	status := checker.Check(context.Background())
	assert.False(t, status.Ready)
	assert.Contains(t, dependenciesByName(status)[RPCName("testnet")].Error, "ledger 0")
}

func TestCheckCachesResultsBriefly(t *testing.T) {
	// A probe every second must not become an RPC call every second.
	var (
		mu    sync.Mutex
		calls int
	)
	now := time.Now()
	checker, err := New(Options{
		Store:  store.NewMemoryStore(),
		Config: twoNetworks(),
		Clients: func(string, config.Network) (stellar.Client, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return stellar.NewMockClient(), nil
		},
		TTL: time.Minute,
		Now: func() time.Time { return now },
	})
	require.NoError(t, err)

	checker.Check(context.Background())
	checker.Check(context.Background())
	checker.Check(context.Background())

	mu.Lock()
	assert.Equal(t, 2, calls, "one refresh should call the factory once per network")
	mu.Unlock()

	// Past the TTL, a new check runs.
	now = now.Add(2 * time.Minute)
	checker.Check(context.Background())

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 4, calls, "an expired result should be refreshed")
}

func TestCheckAppliesTimeout(t *testing.T) {
	checker, err := New(Options{
		Store:  store.NewMemoryStore(),
		Config: &config.Config{Networks: map[string]config.Network{"testnet": {}}},
		Clients: func(string, config.Network) (stellar.Client, error) {
			return slowClient{}, nil
		},
		Timeout: 20 * time.Millisecond,
	})
	require.NoError(t, err)

	status := checker.Check(context.Background())

	assert.False(t, status.Ready)
	assert.Contains(t, dependenciesByName(status)[RPCName("testnet")].Error, "deadline")
}

func TestNewRequiresDependencies(t *testing.T) {
	base := func() Options {
		return Options{Store: store.NewMemoryStore(), Config: twoNetworks(), Clients: healthyFactory()}
	}

	t.Run("store", func(t *testing.T) {
		opts := base()
		opts.Store = nil
		_, err := New(opts)
		assert.ErrorContains(t, err, "store is required")
	})

	t.Run("config", func(t *testing.T) {
		opts := base()
		opts.Config = nil
		_, err := New(opts)
		assert.ErrorContains(t, err, "config is required")
	})

	t.Run("clients", func(t *testing.T) {
		opts := base()
		opts.Clients = nil
		_, err := New(opts)
		assert.ErrorContains(t, err, "client factory is required")
	})
}

func dependenciesByName(status Status) map[string]Dependency {
	out := make(map[string]Dependency, len(status.Dependencies))
	for _, dep := range status.Dependencies {
		out[dep.Name] = dep
	}
	return out
}
