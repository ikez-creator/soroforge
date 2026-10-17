package api

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/soroworks/soroforge/internal/config"
)

// minTokenLength is the shortest API token the server will accept.
//
// This endpoint can deploy contracts with a live signing key, so a token short
// enough to guess is not meaningfully different from no token at all. The limit
// is a floor, not advice: use a long random value.
const minTokenLength = 16

// defaultTokenName labels the token configured through the legacy
// SOROFORGE_API_TOKEN variable in access logs.
const defaultTokenName = "default"

// Scope names a permission. Every route requires one (see the table in
// NewRouter) and every token grants one.
type Scope string

const (
	// ScopePublic marks a route that needs no token at all, such as the
	// liveness and readiness probes.
	ScopePublic Scope = "public"

	// ScopeRead permits GET routes that report state but change nothing.
	ScopeRead Scope = "read"

	// ScopeWrite permits routes that submit transactions. A write token also
	// reads: whoever can deploy can look at the result.
	ScopeWrite Scope = "write"
)

// Token is one bearer credential the API accepts.
//
// The secret is unexported so that it cannot be logged, serialised, or printed
// by accident, in the same spirit as the signing key. Name exists so that
// authenticated requests can be attributed in the access log — "the dashboard
// token deployed this" — without the value ever appearing.
type Token struct {
	Name     string
	readOnly bool
	secret   []byte
}

// String redacts the secret, so that a Token printed with %v or %s cannot leak
// it.
func (t Token) String() string {
	return fmt.Sprintf("Token{Name: %q, readOnly: %t}", t.Name, t.readOnly)
}

// LogValue implements slog.LogValuer for the same reason as String.
func (t Token) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", t.Name),
		slog.Bool("read_only", t.readOnly),
	)
}

// NewToken builds a credential. readOnly tokens are refused on mutating
// routes; read-write tokens can call every route.
func NewToken(name, secret string, readOnly bool) Token {
	return Token{Name: name, secret: []byte(secret), readOnly: readOnly}
}

// Allows reports whether the token grants a route's required scope.
func (t Token) Allows(required Scope) bool {
	switch required {
	case ScopePublic, ScopeRead:
		return true
	case ScopeWrite:
		return !t.readOnly
	default:
		return false
	}
}

// ParseTokens builds the API's credentials from its two environment sources.
//
// legacy is SOROFORGE_API_TOKEN, the original single-token configuration. It
// keeps working unchanged and grants read and write, so existing deployments
// need no migration.
//
// scoped is SOROFORGE_API_TOKENS: a comma-separated list of name:scope:token
// entries, where scope is "read" or "write". Names are labels for logs. A
// malformed entry is an error rather than a silently ignored token — a typo
// that left a token unscoped would be a security-relevant surprise. NewRouter
// applies the remaining rules (minimum length, unique names and values).
func ParseTokens(legacy, scoped string) ([]Token, error) {
	var tokens []Token

	if legacy != "" {
		tokens = append(tokens, NewToken(defaultTokenName, legacy, false))
	}

	for _, entry := range strings.Split(scoped, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		token, err := parseTokenEntry(entry)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}

	return tokens, nil
}

// parseTokenEntry decodes one name:scope:token entry.
//
// SplitN bounds the token at the second colon instead of the last, so a secret
// may contain colons. It may not contain commas, which separate entries.
func parseTokenEntry(entry string) (Token, error) {
	parts := strings.SplitN(entry, ":", 3)
	if len(parts) != 3 {
		return Token{}, fmt.Errorf(
			"%s: each entry must have the form name:scope:token (scope is read or write)",
			config.EnvAPITokens)
	}

	name := strings.TrimSpace(parts[0])
	scope := strings.TrimSpace(parts[1])
	secret := parts[2]

	if name == "" {
		return Token{}, fmt.Errorf("%s: an entry has no name", config.EnvAPITokens)
	}
	if scope != string(ScopeRead) && scope != string(ScopeWrite) {
		// The secret is never echoed; only the parts the user intended as
		// labels are.
		return Token{}, fmt.Errorf(
			"%s: token %q: unknown scope %q: expected %s or %s",
			config.EnvAPITokens, name, scope, ScopeRead, ScopeWrite)
	}

	return NewToken(name, secret, scope == string(ScopeRead)), nil
}

// validateTokens rejects a credential set the server must not start with.
//
// The server refuses to boot on failure rather than starting in an open state.
// An HTTP endpoint that deploys contracts using a signing key should never be
// reachable without authentication, and "we'll put it behind a proxy" is a
// promise that outlives the person who made it.
func validateTokens(tokens []Token) error {
	if len(tokens) == 0 {
		return fmt.Errorf(
			"no API token configured: set %s (read-write) or %s (scoped, name:scope:token). "+
				"The HTTP API can deploy and upgrade contracts with the configured signing key, "+
				"so it will not start unauthenticated",
			config.EnvAPIToken, config.EnvAPITokens)
	}

	names := make(map[string]bool, len(tokens))
	secrets := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		switch {
		case t.Name == "":
			return fmt.Errorf("API token name is required")
		case len(t.secret) < minTokenLength:
			return fmt.Errorf(
				"API token %q is %d characters; use at least %d (e.g. `openssl rand -hex 32`)",
				t.Name, len(t.secret), minTokenLength)
		case names[t.Name]:
			return fmt.Errorf("API token name %q is configured more than once", t.Name)
		case secrets[string(t.secret)]:
			return fmt.Errorf("API token %q duplicates another token's value; each token must be unique", t.Name)
		}
		names[t.Name] = true
		secrets[string(t.secret)] = true
	}
	return nil
}

// authenticator verifies bearer tokens and applies route scopes.
type authenticator struct {
	tokens []Token
}

// match compares presented against every configured token in constant time,
// and deliberately does not stop at the first hit: returning early would leak,
// through response timing, how many tokens precede the one that matched.
func (a *authenticator) match(presented string) (Token, bool) {
	var matched Token
	found := false
	for _, token := range a.tokens {
		if subtle.ConstantTimeCompare([]byte(presented), token.secret) == 1 {
			matched = token
			found = true
		}
	}
	return matched, found
}

// authorize returns middleware that enforces a route's scope.
//
// A missing or invalid token is a 401: the client does not know how to
// authenticate. A valid token that lacks the route's scope is a 403: the
// caller is known and the answer is no. Keeping those distinct matters for
// read-only tokens, which are legitimate credentials that simply cannot
// deploy.
func (a *authenticator) authorize(required Scope, pattern string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if required == ScopePublic {
				next.ServeHTTP(w, r)
				return
			}

			presented, ok := bearerToken(r)
			if !ok {
				unauthorized(w, "missing bearer token")
				return
			}

			token, ok := a.match(presented)
			if !ok {
				unauthorized(w, "invalid bearer token")
				return
			}

			if !token.Allows(required) {
				writeError(w, http.StatusForbidden, fmt.Sprintf(
					"token %q is read-only and cannot call %s %s; use a read-write token",
					token.Name, r.Method, pattern))
				return
			}

			if info := requestInfoFrom(r.Context()); info != nil {
				info.tokenName = token.Name
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the token from an Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}

	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}

	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

func unauthorized(w http.ResponseWriter, detail string) {
	// WWW-Authenticate tells a client how to authenticate, per RFC 7235. It is
	// sent on 401 only: a 403 means the caller is already authenticated.
	w.Header().Set("WWW-Authenticate", `Bearer realm="soroforge"`)
	writeError(w, http.StatusUnauthorized, detail)
}

// requestInfo is mutable state shared down a request's middleware chain.
//
// The access log runs before authentication, so it installs a pointer in the
// request context; authenticate records the matched token name on it. The
// pointer is how the value travels back "up" to the log line after the
// response is written.
type requestInfo struct {
	tokenName string
}

type requestInfoKey struct{}

func withRequestInfo(r *http.Request) (*requestInfo, *http.Request) {
	info := &requestInfo{}
	return info, r.WithContext(context.WithValue(r.Context(), requestInfoKey{}, info))
}

func requestInfoFrom(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(requestInfoKey{}).(*requestInfo)
	return info
}
