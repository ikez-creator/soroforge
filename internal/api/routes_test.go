package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/soroworks/soroforge/internal/readiness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubService satisfies Service without doing anything. No request in this
// file reaches a handler with a body to decode, and routes that would are
// rejected by the auth middleware before dispatch.
type stubService struct{ Service }

// stubReadiness reports ready so /ready is safe to call.
type stubReadiness struct{}

func (stubReadiness) Check(context.Context) readiness.Status {
	return readiness.Status{Ready: true}
}

func buildRouter(t *testing.T) http.Handler {
	t.Helper()

	router, err := NewRouter(Options{
		Service:   &stubService{},
		Tokens:    []Token{NewToken("ci", "0123456789abcdef", false)},
		Readiness: stubReadiness{},
	})
	require.NoError(t, err)
	return router
}

// TestEveryRouteHasAScope is the guard for the route → scope table. A new
// endpoint registered directly on the router, or one added to the table
// without being mounted, fails this test — which is the whole point of keeping
// the table in one place.
func TestEveryRouteHasAScope(t *testing.T) {
	h := &handlers{}
	declared := map[string]Scope{}

	for _, rt := range h.routes() {
		key := rt.method + " " + rt.pattern

		_, duplicate := declared[key]
		require.False(t, duplicate, "route %s is declared twice", key)

		assert.Contains(t,
			[]Scope{ScopePublic, ScopeRead, ScopeWrite}, rt.scope,
			"route %s has an unknown scope %q", key, rt.scope)

		declared[key] = rt.scope
	}

	registered := map[string]bool{}
	err := chi.Walk(buildRouter(t).(*chi.Mux),
		func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			key := method + " " + pattern
			registered[key] = true
			if _, ok := declared[key]; !ok {
				t.Errorf("route %s is mounted but missing from the scope table in handlers.routes(); "+
					"add it with a scope", key)
			}
			return nil
		})
	require.NoError(t, err)

	for key := range declared {
		assert.True(t, registered[key],
			"scope table declares %s but the router does not mount it", key)
	}
}

// TestDeclaredScopesAreEnforced proves each table entry is not merely
// recorded: a scoped route must reject an unauthenticated request with 401,
// while a public one must answer without a token. A route added with the wrong
// wrapping would fail here.
func TestDeclaredScopesAreEnforced(t *testing.T) {
	h := &handlers{}
	router := buildRouter(t)

	for _, rt := range h.routes() {
		t.Run(rt.method+" "+rt.pattern, func(t *testing.T) {
			path := concretePath(rt.pattern)
			req := httptest.NewRequest(rt.method, path, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rt.scope == ScopePublic {
				assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
					"a public route must not require a token")
				return
			}
			assert.Equal(t, http.StatusUnauthorized, rec.Code,
				"a scoped route must reject a request with no token")
		})
	}
}

// concretePath replaces chi path parameters with values a request can use.
func concretePath(pattern string) string {
	replacer := strings.NewReplacer("{network}", "testnet", "{alias}", "counter")
	return replacer.Replace(pattern)
}
