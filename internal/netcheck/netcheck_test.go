package netcheck

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The pure parts — URL redaction and passphrase comparison — are the security
// and correctness surfaces of `soroforge networks`, and they need no network
// to prove. The probe cycle is exercised against fake clients below; live
// verification belongs to the SOROFORGE_LIVE suite in internal/stellar.

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain url passes through",
			in:   "https://soroban-testnet.stellar.org",
			want: "https://soroban-testnet.stellar.org",
		},
		{
			name: "userinfo is removed",
			in:   "https://user:hunter2@soroban.example.com/rpc",
			want: "https://soroban.example.com/rpc",
		},
		{
			name: "query string is removed",
			in:   "https://rpc.example.com/v2?api_key=sk-live-12345&network=testnet",
			want: "https://rpc.example.com/v2",
		},
		{
			name: "both removed, port and path kept",
			in:   "http://admin:s3cret@localhost:8000/soroban/rpc?key=xyz",
			want: "http://localhost:8000/soroban/rpc",
		},
		{
			name: "empty stays empty",
			in:   "",
			want: "",
		},
		{
			name: "unparsable url with credentials is still redacted",
			in:   "https://user:pass@exa mple.org/rpc?token=abc",
			want: "exa mple.org/rpc",
		},
		{
			name: "localhost without scheme is untouched when it holds no secrets",
			in:   "localhost:8000/soroban/rpc",
			want: "localhost:8000/soroban/rpc",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURL(tc.in)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "hunter2")
			assert.NotContains(t, got, "sk-live")
			assert.NotContains(t, got, "s3cret")
			assert.NotContains(t, got, "token=abc")
		})
	}
}

func TestPassphrasesMatch(t *testing.T) {
	testnet := "Test SDF Network ; September 2015"

	assert.True(t, PassphrasesMatch(testnet, testnet))
	assert.True(t, PassphrasesMatch(testnet, testnet+" "),
		"a trailing space from YAML or an old RPC response is still the same network")
	assert.True(t, PassphrasesMatch(" "+testnet, testnet))
	assert.False(t, PassphrasesMatch(testnet, "Public Global Stellar Network ; September 2015"),
		"a wrong network must never compare equal")
	assert.False(t, PassphrasesMatch(testnet, "Test SDF Network ; September 2016"))
	assert.False(t, PassphrasesMatch(testnet, ""), "an empty report is not a match")
	assert.False(t, PassphrasesMatch("", testnet), "an unconfigured passphrase cannot match")
	// Edge cases:
	assert.False(t, PassphrasesMatch("   ", "   "), "two whitespace-only strings must not match after TrimSpace")
	assert.True(t, PassphrasesMatch("Test SDF Network ; September 2015", "Test SDF Network ; September 2015   \t\n"), "trailing whitespace+newlines trimmed")
	assert.False(t, PassphrasesMatch("Test SDF Network ; September 2015", "Test SDF Network ; September 2015 extra"), "internal extra text must not match")
}

// stubClient is a stellar.Client whose endpoint behaviour tests choose. It
// deliberately does NOT implement stellar.NetworkReporter, to model an RPC
// without getNetwork support.
type stubClient struct {
	ledger    uint32
	ledgerErr error
	latency   time.Duration
}

var _ stellar.Client = (*stubClient)(nil)

func (s *stubClient) NetworkPassphrase() string { return "configured-elsewhere" }
func (s *stubClient) LoadAccount(context.Context, string) (txnbuild.Account, error) {
	panic("unused by netcheck")
}
func (s *stubClient) Simulate(context.Context, string) (stellar.SimulateResult, error) {
	panic("unused")
}
func (s *stubClient) NetworkInfo(context.Context) (string, error)              { panic("unused by netcheck") }
func (s *stubClient) Send(context.Context, string) (stellar.SendResult, error) { panic("unused") }
func (s *stubClient) AwaitTransaction(context.Context, string) (stellar.TxResult, error) {
	panic("unused")
}
func (s *stubClient) LedgerEntries(context.Context, []string) ([]stellar.LedgerEntry, error) {
	panic("unused")
}
func (s *stubClient) LatestLedger(ctx context.Context) (uint32, error) {
	if s.latency > 0 {
		select {
		case <-time.After(s.latency):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if s.ledgerErr != nil {
		return 0, s.ledgerErr
	}
	return s.ledger, nil
}

// reportedClient layers getNetwork behaviour on top of stubClient, modelling
// the endpoint a healthy configured network points at.
type reportedClient struct {
	stubClient
	passphrase string
	reportErr  error
}

var _ stellar.NetworkReporter = (*reportedClient)(nil)

func (r *reportedClient) ReportNetworkPassphrase(context.Context) (string, error) {
	if r.reportErr != nil {
		return "", r.reportErr
	}
	return r.passphrase, nil
}

func factoryFor(clients map[string]stellar.Client) ClientFactory {
	return func(name string, _ config.Network) (stellar.Client, error) {
		c, ok := clients[name]
		if !ok {
			return nil, errors.New("connect refused")
		}
		return c, nil
	}
}

func TestCheckReportsHealthyNetwork(t *testing.T) {
	networks := map[string]config.Network{
		"testnet": {
			RPCURL:     "https://user:token@rpc.example.com/soroban?api_key=abc",
			Passphrase: "Test SDF Network ; September 2015",
		},
	}
	clients := map[string]stellar.Client{
		"testnet": &reportedClient{
			stubClient: stubClient{ledger: 50_000},
			passphrase: "Test SDF Network ; September 2015",
		},
	}

	results := Check(context.Background(), networks, factoryFor(clients), time.Second)
	require.Len(t, results, 1)
	r := results[0]

	assert.True(t, r.OK)
	assert.True(t, r.Responded)
	assert.Equal(t, uint32(50_000), r.LatestLedger)
	assert.True(t, r.PassphraseVerified)
	assert.True(t, r.PassphraseMatch)
	assert.Empty(t, r.Error)
	assert.Equal(t, "https://rpc.example.com/soroban", r.RedactedURL,
		"credentials in the URL must never survive into the result")
}

// RPC drivers interpolate the request URL into their error messages, and
// masking is inconsistent: the stellar SDK's jrpc2 layer shows the username
// and the query string verbatim while masking only the password. A failed
// probe's Error is printed by `soroforge networks` and shipped in --json, so
// every URL embedded in it must be scrubbed before it leaves checkOne.
func TestCheckScrubsURLsFromErrorText(t *testing.T) {
	networks := map[string]config.Network{
		"testnet": {
			RPCURL:     "https://user:secret-token@dead.example.test/soroban?api_key=sup3rsecret",
			Passphrase: "Test SDF Network ; September 2015",
		},
	}
	clients := map[string]stellar.Client{
		// Password masked by the driver, username and key not.
		"masked": &stubClient{ledgerErr: errors.New(
			`Post "https://user:***@dead.example.test/soroban?api_key=sup3rsecret": dial tcp: lookup dead.example.test`)},
		// Raw URL verbatim, including the actual password.
		"raw": &stubClient{ledgerErr: errors.New(
			`dial https://user:secret-token@dead.example.test/soroban?api_key=sup3rsecret: no such host`)},
	}

	for _, name := range []string{"masked", "raw"} {
		nets := map[string]config.Network{"testnet": networks["testnet"]}
		results := Check(context.Background(), nets,
			factoryFor(map[string]stellar.Client{"testnet": clients[name]}), time.Second)
		require.Len(t, results, 1)
		r := results[0]

		assert.False(t, r.OK)
		assert.NotContains(t, r.Error, "api_key=sup3rsecret", "query-string credentials must not reach the error")
		assert.NotContains(t, r.Error, "secret-token", "password must not reach the error")
		assert.NotContains(t, r.Error, "user:", "userinfo must not reach the error")
		assert.Contains(t, r.Error, "https://dead.example.test/soroban",
			"the host must survive: the operator needs to know which endpoint failed")
		assert.Equal(t, "https://dead.example.test/soroban", r.RedactedURL)
	}
}

func TestRedactURLsInFreeText(t *testing.T) {
	got := redactURLsIn(`getNetwork: Post "https://a:b@host.example/v1?token=xyz": context deadline exceeded`)
	assert.Equal(t, `getNetwork: Post "https://host.example/v1": context deadline exceeded`, got)

	// Text without any URL is returned untouched.
	assert.Equal(t, "RPC returned ledger 0", redactURLsIn("RPC returned ledger 0"))
}

func TestCheckFlagsWrongNetwork(t *testing.T) {
	// The failure this command exists for: the URL answers fine, but it
	// serves a different network than the passphrase claims.
	networks := map[string]config.Network{
		"mainnet": {
			RPCURL:     "https://rpc.example.com",
			Passphrase: "Public Global Stellar Network ; September 2015",
		},
	}
	clients := map[string]stellar.Client{
		"mainnet": &reportedClient{
			stubClient: stubClient{ledger: 10},
			passphrase: "Test SDF Network ; September 2015",
		},
	}

	results := Check(context.Background(), networks, factoryFor(clients), time.Second)
	require.Len(t, results, 1)
	r := results[0]

	assert.False(t, r.OK)
	assert.True(t, r.Responded, "the endpoint did answer; the configuration is what is wrong")
	assert.False(t, r.PassphraseMatch)
	assert.ErrorContains(t, errors.New(r.Error), "does not match")
}

func TestCheckHandlesUnreachableAndUnsupportedEndpoints(t *testing.T) {
	networks := map[string]config.Network{
		"noreport":    {RPCURL: "https://noreport.example.com", Passphrase: "P"},
		"getnet":      {RPCURL: "https://getnet.example.com", Passphrase: "P"},
		"zero":        {RPCURL: "https://zero.example.com", Passphrase: "P"},
		"unreachable": {RPCURL: "https://unreachable.example.com", Passphrase: "P"},
		"slow":        {RPCURL: "https://slow.example.com", Passphrase: "P"},
	}
	clients := map[string]stellar.Client{
		// Answers, but its getNetwork fails: the passphrase stays unverified.
		"getnet": &reportedClient{stubClient: stubClient{ledger: 7}, reportErr: errors.New("getNetwork: unsupported")},
		// Answers, but is not a NetworkReporter at all.
		"noreport":    &stubClient{ledger: 7},
		"zero":        &stubClient{ledger: 0},
		"unreachable": &stubClient{ledgerErr: errors.New("dial tcp: no route to host")},
		"slow":        &stubClient{ledger: 9, latency: 200 * time.Millisecond},
	}

	results := Check(context.Background(), networks, factoryFor(clients), 50*time.Millisecond)
	byName := map[string]Result{}
	for _, r := range results {
		byName[r.Network] = r
	}
	require.Len(t, results, 5)

	assert.False(t, byName["getnet"].OK)
	assert.Contains(t, byName["getnet"].Error, "passphrase")
	assert.False(t, byName["getnet"].PassphraseVerified)

	assert.False(t, byName["noreport"].OK)
	assert.False(t, byName["noreport"].PassphraseVerified)

	assert.False(t, byName["zero"].OK)
	assert.True(t, byName["zero"].Responded, "the endpoint answered; the answer was nonsense")

	assert.False(t, byName["unreachable"].OK)
	assert.False(t, byName["unreachable"].Responded)

	assert.False(t, byName["slow"].OK, "a probe over the timeout counts as a failure")
	assert.False(t, byName["slow"].Responded)

	// Output is sorted by network name so reports diff cleanly.
	var names []string
	for _, r := range results {
		names = append(names, r.Network)
	}
	assert.Equal(t, []string{"getnet", "noreport", "slow", "unreachable", "zero"}, names)
}

func TestCheckContinuesWhenOneNetworkCannotBeCreated(t *testing.T) {
	networks := map[string]config.Network{
		"good": {RPCURL: "https://good.example.com", Passphrase: "Test SDF Network ; September 2015"},
		"bad":  {RPCURL: "https://bad.example.com", Passphrase: "P"},
	}
	clients := map[string]stellar.Client{
		"good": &reportedClient{stubClient: stubClient{ledger: 3}, passphrase: "Test SDF Network ; September 2015"},
	}

	results := Check(context.Background(), networks, factoryFor(clients), time.Second)
	require.Len(t, results, 2)

	var good, bad Result
	for _, r := range results {
		if r.Network == "good" {
			good = r
		} else {
			bad = r
		}
	}
	assert.True(t, good.OK, "one broken network must not suppress the other's report")
	assert.False(t, bad.OK)
	assert.Equal(t, "connect refused", bad.Error)
}

func TestCheckEmptyConfig(t *testing.T) {
	tests := []struct {
		name     string
		networks map[string]config.Network
	}{
		{name: "nil network map", networks: nil},
		{name: "empty network map", networks: map[string]config.Network{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			results := Check(context.Background(), tc.networks, factoryFor(nil), time.Second)
			assert.Empty(t, results)
		})
	}
}

// Check's edge cases decide whether a report is a failure or an empty sweep:
// a timeout that must fall back to the default, and the largest ledger
// sequence an endpoint can report.
func TestCheckEdgeCases(t *testing.T) {
	tests := []struct {
		name         string
		networks     map[string]config.Network
		clients      map[string]stellar.Client
		timeout      time.Duration
		wantNetworks []string
		wantLedgers  []uint32
	}{
		{
			name: "zero timeout falls back to the default",
			networks: map[string]config.Network{
				"testnet": {RPCURL: "https://rpc.example.com", Passphrase: "P"},
			},
			// The delay only completes under DefaultTimeout; a literal zero
			// timeout would cancel the probe before it answers.
			clients: map[string]stellar.Client{
				"testnet": &reportedClient{
					stubClient: stubClient{ledger: 1, latency: time.Millisecond},
					passphrase: "P",
				},
			},
			timeout:      0,
			wantNetworks: []string{"testnet"},
			wantLedgers:  []uint32{1},
		},
		{
			name: "negative timeout falls back to the default",
			networks: map[string]config.Network{
				"testnet": {RPCURL: "https://rpc.example.com", Passphrase: "P"},
			},
			clients: map[string]stellar.Client{
				"testnet": &reportedClient{
					stubClient: stubClient{ledger: 2, latency: time.Millisecond},
					passphrase: "P",
				},
			},
			timeout:      -time.Second,
			wantNetworks: []string{"testnet"},
			wantLedgers:  []uint32{2},
		},
		{
			name: "maximum ledger sequence is accepted",
			networks: map[string]config.Network{
				"testnet": {RPCURL: "https://rpc.example.com", Passphrase: "P"},
			},
			clients: map[string]stellar.Client{
				"testnet": &reportedClient{
					stubClient: stubClient{ledger: math.MaxUint32},
					passphrase: "P",
				},
			},
			timeout:      time.Second,
			wantNetworks: []string{"testnet"},
			wantLedgers:  []uint32{math.MaxUint32},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			results := Check(context.Background(), tc.networks, factoryFor(tc.clients), tc.timeout)
			require.Len(t, results, len(tc.wantNetworks))
			for i, r := range results {
				assert.Equal(t, tc.wantNetworks[i], r.Network)
				assert.Equal(t, tc.wantLedgers[i], r.LatestLedger)
				assert.True(t, r.OK, "a valid boundary case must still pass")
				assert.Empty(t, r.Error)
			}
		})
	}
}

// AnyFailure is what the CLI uses to choose its exit code.
func TestAnyFailure(t *testing.T) {
	tests := []struct {
		name    string
		results []Result
		want    bool
	}{
		{name: "nil results", results: nil, want: false},
		{name: "empty results", results: []Result{}, want: false},
		{name: "all passing", results: []Result{{OK: true}, {OK: true}}, want: false},
		{name: "one failure", results: []Result{{OK: true}, {OK: false, Error: "boom"}}, want: true},
		{name: "zero-value result fails closed", results: []Result{{}}, want: true},
		{name: "failure first", results: []Result{{Error: "boom"}, {OK: true}}, want: true},
		{name: "all failing", results: []Result{{Error: "a"}, {Error: "b"}}, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, AnyFailure(tc.results))
		})
	}
}
