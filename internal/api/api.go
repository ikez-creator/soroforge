// Package api exposes SoroForge's operations over HTTP so that CI pipelines can
// drive deploys without a shell.
//
// The handlers are a thin layer over the same deploy.Service the CLI uses —
// there is no lifecycle logic here, only request decoding, authentication, and
// JSON encoding. Anything that changes contract behaviour belongs in the deploy
// package, where it is exercised by both surfaces.
//
// Mutating endpoints require a bearer token and the server refuses to start
// without one; see auth.go. Every route declares the scope it needs in the
// table returned by handlers.routes, and a test walks the built router to
// prove no route was added around that table.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/readiness"
	"github.com/soroworks/soroforge/internal/store"
)

// Service is the subset of deploy.Service the API needs.
//
// Declaring it here rather than taking the concrete type keeps the handlers
// testable against a fake and documents exactly which operations are reachable
// over HTTP.
type Service interface {
	Deploy(ctx context.Context, req deploy.DeployRequest) (*deploy.DeployResult, error)
	Upgrade(ctx context.Context, req deploy.UpgradeRequest) (*deploy.UpgradeResult, error)
	Status(ctx context.Context, req deploy.StatusRequest) (*deploy.StatusResult, error)
	StatusAll(ctx context.Context, network string) (*deploy.NetworkStatus, error)
	List(ctx context.Context, network string) ([]store.Contract, error)
	History(ctx context.Context, network, alias string) ([]store.Deployment, error)
}

// Readiness reports whether SoroForge's dependencies are usable. It backs
// GET /ready.
//
// It is declared here as a one-method interface so the handlers cannot reach
// past it into a database or RPC client: the API package checks dependencies
// through this seam, never directly.
type Readiness interface {
	Check(ctx context.Context) readiness.Status
}

// requestTimeout bounds a single API request.
//
// A deploy waits for two transactions to be confirmed, so this has to be
// generous — but it must exist, or a stuck network holds a connection open
// indefinitely.
const requestTimeout = 5 * time.Minute

// Options configures the HTTP server.
type Options struct {
	// Service performs the work. Required.
	Service Service

	// Tokens are the bearer credentials the API accepts. At least one is
	// required; the server refuses to start without one.
	Tokens []Token

	// Readiness checks dependencies for GET /ready. Required.
	Readiness Readiness

	// Log defaults to slog.Default().
	Log *slog.Logger
}

// route is one endpoint, the scope a token must hold to call it, and its
// handler.
type route struct {
	method  string
	pattern string
	scope   Scope
	handler http.HandlerFunc
}

// routes returns every endpoint the API serves, each with its required scope.
//
// This table is the single place route permissions are declared: NewRouter
// mounts exactly these entries, wrapping each in the middleware for its scope,
// and TestEveryRouteHasAScope walks the finished router to prove nothing was
// added around it. A new endpoint that forgets to declare its scope fails the
// test rather than silently inheriting access.
//
// ScopePublic routes (/health and /ready) deliberately take no token: a
// monitoring system should not need to hold a credential that can deploy in
// order to ask whether the process is alive or ready.
func (h *handlers) routes() []route {
	return []route{
		{http.MethodGet, "/health", ScopePublic, h.health},
		{http.MethodGet, "/ready", ScopePublic, h.ready},

		{http.MethodPost, "/v1/deploy", ScopeWrite, h.deploy},
		{http.MethodPost, "/v1/upgrade", ScopeWrite, h.upgrade},

		{http.MethodGet, "/v1/contracts", ScopeRead, h.listContracts},
		{http.MethodGet, "/v1/contracts/{network}/{alias}/history", ScopeRead, h.history},
		{http.MethodGet, "/v1/contracts/{network}/{alias}/status", ScopeRead, h.status},
		{http.MethodGet, "/v1/contracts/{network}/status", ScopeRead, h.statusAll},
	}
}

// NewRouter builds the HTTP handler.
//
// It returns an error rather than a router when the tokens are unusable or a
// required dependency is missing, so that a misconfigured server fails at
// startup instead of serving unauthenticated deploy endpoints.
func NewRouter(opts Options) (http.Handler, error) {
	if opts.Service == nil {
		return nil, fmt.Errorf("api: service is required")
	}
	if err := validateTokens(opts.Tokens); err != nil {
		return nil, err
	}
	if opts.Readiness == nil {
		return nil, fmt.Errorf("api: readiness is required")
	}

	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	h := &handlers{svc: opts.Service, readiness: opts.Readiness, log: log}
	auth := &authenticator{tokens: opts.Tokens}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// The access log wraps the recoverer so a panic's 500 is logged with its
	// status like any other response.
	r.Use(accessLog(log))
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(requestTimeout))

	for _, rt := range h.routes() {
		r.With(auth.authorize(rt.scope, rt.pattern)).Method(rt.method, rt.pattern, rt.handler)
	}

	return r, nil
}

// accessLog returns middleware that writes one structured line per request.
//
// It never logs headers or bodies: the Authorization header carries the bearer
// token, and a deploy body can contain anything. Logging the matched token's
// name is enough to answer "who deployed this" without the value appearing
// anywhere.
//
// Requests to /health are logged at debug level so that a liveness probe
// polling every second cannot bury real traffic.
func accessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info, r := withRequestInfo(r)

			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				// The handler wrote nothing — the implicit status is 200.
				status = http.StatusOK
			}

			pattern := routePattern(r)
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("route", pattern),
				slog.Int("status", status),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
				slog.String("remote_addr", r.RemoteAddr),
			}
			if info.tokenName != "" {
				attrs = append(attrs, slog.String("token", info.tokenName))
			}

			level := slog.LevelInfo
			if pattern == "/health" {
				level = slog.LevelDebug
			}
			log.LogAttrs(r.Context(), level, "http request", attrs...)
		})
	}
}

// routePattern returns the matched chi pattern, so logs say
// /v1/contracts/{network}/{alias}/status rather than one line per path. For a
// request that matched nothing, the raw path is the only useful answer.
func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return r.URL.Path
}

// NewServer builds an http.Server with sensible timeouts.
func NewServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: handler,
		// ReadHeaderTimeout bounds how long a client may take to send headers,
		// which is the cheap defence against a Slowloris-style connection hold.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout must outlast requestTimeout so a slow deploy still gets
		// to write its response rather than having the connection cut.
		WriteTimeout: requestTimeout + 30*time.Second,
		IdleTimeout:  120 * time.Second,
	}
}

type handlers struct {
	svc       Service
	readiness Readiness
	log       *slog.Logger
}

// errorResponse is the body returned for every failure, so clients can parse
// one shape.
type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	// The status line is already sent, so an encoding failure cannot be turned
	// into an error response; log it at the call site's discretion instead.
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

// decodeJSON reads a request body, rejecting unknown fields so that a
// misspelled key is a clear 400 rather than a silently ignored option — the
// same reasoning as KnownFields in the config loader.
func decodeJSON(r *http.Request, dest any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

// statusForError maps an operation error to an HTTP status.
//
// Configuration mistakes are the client's fault (400) and a missing record is a
// 404; anything else is reported as a server error, since SoroForge cannot tell
// a network failure from a bug.
func statusForError(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case isClientError(err):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// clientErrorMarkers are substrings of the errors produced by bad request
// input, as opposed to network or database failures.
//
// Matching on message text is unlovely. It is here because these errors come
// from config lookups that predate any need for HTTP status codes, and adding
// sentinel error types across three packages to serve one status decision is a
// worse trade at this size. If this list grows much, that is the signal to
// introduce typed errors in the config and deploy packages.
var clientErrorMarkers = []string{
	"unknown contract",
	"unknown network",
	"not tracked",
	"no signing key configured",
	"invalid request body",
	"is required",
}

// isClientError recognises the errors produced by bad request input.
func isClientError(err error) bool {
	msg := err.Error()
	for _, marker := range clientErrorMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
