package deploy_test

import (
	"context"
	"sync"
	"testing"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Drift detection is what makes the recorded history trustworthy: it tests
// SoroForge's claim against the ledger rather than assuming the database is
// right. These cases cover each way that comparison can come out.

func TestStatusResultInSync(t *testing.T) {
	tests := map[string]struct {
		result deploy.StatusResult
		want   bool
	}{
		"in sync": {
			result: deploy.StatusResult{State: deploy.StateInSync},
			want:   true,
		},
		"zero value has no state": {
			result: deploy.StatusResult{},
			want:   false,
		},
		"drift": {
			result: deploy.StatusResult{State: deploy.StateDrift},
			want:   false,
		},
		"untracked": {
			result: deploy.StatusResult{State: deploy.StateUntracked},
			want:   false,
		},
		"missing": {
			result: deploy.StatusResult{State: deploy.StateMissing},
			want:   false,
		},
		"archived": {
			result: deploy.StatusResult{State: deploy.StateArchived},
			want:   false,
		},
		"unknown state": {
			result: deploy.StatusResult{State: deploy.State("something_new")},
			want:   false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.result.InSync())
		})
	}
}

func TestStatusInSync(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	trackContract(t, h, h.wasmHash)

	result, err := h.svc.Status(context.Background(), deploy.StatusRequest{Alias: "counter"})
	require.NoError(t, err)

	assert.Equal(t, deploy.StateInSync, result.State)
	assert.True(t, result.InSync())
	assert.Equal(t, result.ExpectedWasmHash, result.ActualWasmHash)
	assert.Equal(t, h.contractID, result.ContractID)
}

func TestStatusDetectsDrift(t *testing.T) {
	// Someone upgraded the contract outside SoroForge. The database says one
	// thing, the chain says another, and the tool must report the chain.
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	recorded := stellar.WasmHash([]byte("what soroforge deployed"))
	live := stellar.WasmHash([]byte("what is actually running"))

	_, err := h.store.UpsertContract(ctx, store.Contract{
		Alias: "counter", Network: "testnet", ContractID: h.contractID,
		CurrentWasmHash: stellar.HashHex(recorded),
	})
	require.NoError(t, err)
	require.NoError(t, h.client.SetContractInstance(h.contractID, live))

	result, err := h.svc.Status(ctx, deploy.StatusRequest{Alias: "counter"})
	require.NoError(t, err)

	assert.Equal(t, deploy.StateDrift, result.State)
	assert.False(t, result.InSync())
	assert.Equal(t, stellar.HashHex(recorded), result.ExpectedWasmHash)
	assert.Equal(t, stellar.HashHex(live), result.ActualWasmHash)
	assert.Contains(t, result.Detail, "outside SoroForge")
}

func TestStatusUntracked(t *testing.T) {
	// SoroForge has no record of this contract on this network.
	h := newHarness(t, harnessOptions{})

	result, err := h.svc.Status(context.Background(), deploy.StatusRequest{Alias: "counter"})
	require.NoError(t, err)

	assert.Equal(t, deploy.StateUntracked, result.State)
	assert.False(t, result.InSync())
	assert.Empty(t, result.ContractID)
	assert.Contains(t, result.Detail, "not tracked")
}

func TestStatusMissingOnChain(t *testing.T) {
	// SoroForge has a record, but the ledger has no such contract — it was
	// never deployed to this network, or its instance entry expired. That is a
	// different problem from drift and must not be reported as one.
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	_, err := h.store.UpsertContract(ctx, store.Contract{
		Alias: "counter", Network: "testnet", ContractID: h.contractID,
		CurrentWasmHash: stellar.HashHex(h.wasmHash),
	})
	require.NoError(t, err)
	// Deliberately no ledger entry.

	result, err := h.svc.Status(ctx, deploy.StatusRequest{Alias: "counter"})
	require.NoError(t, err)

	assert.Equal(t, deploy.StateMissing, result.State)
	assert.False(t, result.InSync())
	assert.Contains(t, result.Detail, "no instance")
}

func TestStatusArchived(t *testing.T) {
	// An archived contract still exists, so it is neither missing nor in
	// sync: it needs a restore before it can be called again, and the status
	// must say so rather than reporting drift or absence.
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	_, err := h.store.UpsertContract(ctx, store.Contract{
		Alias: "counter", Network: "testnet", ContractID: h.contractID,
		CurrentWasmHash: stellar.HashHex(h.wasmHash),
	})
	require.NoError(t, err)
	require.NoError(t, h.client.SetContractInstanceArchived(h.contractID, h.wasmHash))

	result, err := h.svc.Status(ctx, deploy.StatusRequest{Alias: "counter"})
	require.NoError(t, err)

	assert.Equal(t, deploy.StateArchived, result.State)
	assert.False(t, result.InSync())
	assert.Contains(t, result.Detail, "archived")
	assert.Contains(t, result.Detail, "soroforge restore counter")
}

func TestStatusIsPerNetwork(t *testing.T) {
	// A contract in sync on testnet says nothing about mainnet.
	h := newHarness(t, harnessOptions{})
	trackContract(t, h, h.wasmHash)

	ctx := context.Background()

	testnet, err := h.svc.Status(ctx, deploy.StatusRequest{Alias: "counter", Network: "testnet"})
	require.NoError(t, err)
	assert.Equal(t, deploy.StateInSync, testnet.State)

	mainnet, err := h.svc.Status(ctx, deploy.StatusRequest{Alias: "counter", Network: "mainnet"})
	require.NoError(t, err)
	assert.Equal(t, deploy.StateUntracked, mainnet.State)
}

func TestStatusNeedsNoSigner(t *testing.T) {
	// Checking status is a read; requiring a signing key for it would mean
	// exposing a key to run a health check in CI.
	h := newHarness(t, harnessOptions{noSigner: true})
	trackContract(t, h, h.wasmHash)

	result, err := h.svc.Status(context.Background(), deploy.StatusRequest{Alias: "counter"})
	require.NoError(t, err)
	assert.Equal(t, deploy.StateInSync, result.State)
}

func TestStatusRejectsUnknownContract(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	_, err := h.svc.Status(context.Background(), deploy.StatusRequest{Alias: "ghost"})
	assert.ErrorContains(t, err, "unknown contract")
}

func TestStatusEdgeCases(t *testing.T) {
	// Status operates on named config entries, so its edge cases are the
	// requests that cannot resolve: an empty or whitespace-only alias and a
	// network that is not configured.
	h := newHarness(t, harnessOptions{})

	tests := []struct {
		name    string
		req     deploy.StatusRequest
		wantErr string
	}{
		{
			name:    "empty alias",
			req:     deploy.StatusRequest{},
			wantErr: "unknown contract",
		},
		{
			name:    "whitespace-only alias",
			req:     deploy.StatusRequest{Alias: "   "},
			wantErr: "unknown contract",
		},
		{
			name:    "unknown network",
			req:     deploy.StatusRequest{Alias: "counter", Network: "staging"},
			wantErr: "unknown network",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := h.svc.Status(context.Background(), tc.req)
			require.Error(t, err)
			assert.Nil(t, result)
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestListReturnsTrackedContracts(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	for _, c := range []store.Contract{
		{Alias: "counter", Network: "testnet", ContractID: "C1", CurrentWasmHash: "a"},
		{Alias: "counter", Network: "mainnet", ContractID: "C2", CurrentWasmHash: "b"},
	} {
		_, err := h.store.UpsertContract(ctx, c)
		require.NoError(t, err)
	}

	all, err := h.svc.List(ctx, "")
	require.NoError(t, err)
	assert.Len(t, all, 2, "an empty network lists every network")

	testnet, err := h.svc.List(ctx, "testnet")
	require.NoError(t, err)
	require.Len(t, testnet, 1)
	assert.Equal(t, "C1", testnet[0].ContractID)
}

func TestListRejectsUnknownNetwork(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	_, err := h.svc.List(context.Background(), "staging")
	assert.ErrorContains(t, err, "unknown network")
}

func TestHistoryReturnsFullTimeline(t *testing.T) {
	// The history is the product: a deploy followed by two upgrades should read
	// back as a version timeline, newest first.
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	for _, d := range []store.Deployment{
		{
			Alias: "counter", Network: "testnet", ContractID: h.contractID, WasmHash: "v1",
			Action: store.ActionDeploy, DeployerPubkey: "GABC", Actor: "cli",
		},
		{
			Alias: "counter", Network: "testnet", ContractID: h.contractID, WasmHash: "v2",
			Action: store.ActionUpgrade, DeployerPubkey: "GABC", Actor: "cli",
		},
		{
			Alias: "counter", Network: "testnet", ContractID: h.contractID, WasmHash: "v3",
			Action: store.ActionUpgrade, DeployerPubkey: "GDEF", Actor: "cli",
		},
	} {
		_, err := h.store.RecordDeployment(ctx, d)
		require.NoError(t, err)
	}

	history, err := h.svc.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, history, 3)

	assert.Equal(t, "v3", history[0].WasmHash)
	assert.Equal(t, "GDEF", history[0].DeployerPubkey)
	assert.Equal(t, store.ActionDeploy, history[2].Action)
}

func TestHistoryDefaultsToConfiguredNetwork(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	_, err := h.store.RecordDeployment(ctx, store.Deployment{
		Alias: "counter", Network: "testnet", ContractID: h.contractID, WasmHash: "v1",
		Action: store.ActionDeploy, DeployerPubkey: "GABC", Actor: "cli",
	})
	require.NoError(t, err)

	history, err := h.svc.History(ctx, "", "counter")
	require.NoError(t, err)
	assert.Len(t, history, 1)
}

func TestNewServiceValidatesRequiredDependencies(t *testing.T) {
	_, err := deploy.New(deploy.Options{})
	assert.ErrorContains(t, err, "config is required")

	_, err = deploy.New(deploy.Options{Config: newHarness(t, harnessOptions{}).svc.Config()})
	assert.ErrorContains(t, err, "store is required")
}

func TestServiceCachesClientsPerNetworkConcurrentlyAndClosesThem(t *testing.T) {
	h := newHarness(t, harnessOptions{noSigner: true})
	var mu sync.Mutex
	counts := map[string]int{}
	clients := map[string]*stellar.MockClient{}

	svc, err := deploy.New(deploy.Options{
		Config: h.cfg,
		Store:  h.store,
		Clients: func(name string, _ config.Network) (stellar.Client, error) {
			mu.Lock()
			defer mu.Unlock()
			counts[name]++
			client := stellar.NewMockClient()
			clients[name] = client
			return client, nil
		},
	})
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for _, network := range []string{"testnet", "mainnet"} {
		for range 20 {
			wg.Add(1)
			go func(network string) {
				defer wg.Done()
				_, err := svc.Status(context.Background(), deploy.StatusRequest{
					Alias: "counter", Network: network,
				})
				if err != nil {
					errs <- err
				}
			}(network)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	assert.Equal(t, map[string]int{"testnet": 1, "mainnet": 1}, counts)
	require.NoError(t, svc.Close())
	require.NoError(t, svc.Close(), "closing is idempotent")
	assert.Equal(t, 1, clients["testnet"].CloseCalls)
	assert.Equal(t, 1, clients["mainnet"].CloseCalls)

	_, err = svc.Status(context.Background(), deploy.StatusRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "service is closed")
}

func TestStatusAllChecksEveryTrackedContract(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	// counter is in sync; retired was removed from soroforge.yaml but is still
	// tracked and has drifted; gone is tracked but has no instance on-chain.
	trackContract(t, h, h.wasmHash)

	retiredID := otherContractID(t, 2)
	recorded := stellar.WasmHash([]byte("recorded"))
	_, err := h.store.UpsertContract(ctx, store.Contract{
		Alias: "retired", Network: "testnet", ContractID: retiredID, CurrentWasmHash: stellar.HashHex(recorded),
	})
	require.NoError(t, err)
	require.NoError(t, h.client.SetContractInstance(retiredID, stellar.WasmHash([]byte("changed"))))

	_, err = h.store.UpsertContract(ctx, store.Contract{
		Alias: "gone", Network: "testnet", ContractID: otherContractID(t, 3), CurrentWasmHash: "aa",
	})
	require.NoError(t, err)

	// A mainnet record must not appear in a testnet check.
	_, err = h.store.UpsertContract(ctx, store.Contract{
		Alias: "counter", Network: "mainnet", ContractID: otherContractID(t, 4), CurrentWasmHash: "bb",
	})
	require.NoError(t, err)

	result, err := h.svc.StatusAll(ctx, "testnet")
	require.NoError(t, err)

	assert.Equal(t, "testnet", result.Network)
	assert.False(t, result.InSync)
	require.Len(t, result.Contracts, 3)

	states := map[string]deploy.State{}
	var order []string
	for _, c := range result.Contracts {
		states[c.Alias] = c.State
		order = append(order, c.Alias)
	}
	assert.Equal(t, []string{"counter", "gone", "retired"}, order, "results are in alias order")
	assert.Equal(t, deploy.StateInSync, states["counter"])
	assert.Equal(t, deploy.StateMissing, states["gone"])
	assert.Equal(t, deploy.StateDrift, states["retired"], "an alias missing from the config is still checked")
}

func TestStatusAllInSync(t *testing.T) {
	h := newHarness(t, harnessOptions{noSigner: true})
	trackContract(t, h, h.wasmHash)

	result, err := h.svc.StatusAll(context.Background(), "")
	require.NoError(t, err)
	assert.True(t, result.InSync)
	assert.Equal(t, "testnet", result.Network, "an empty network uses default_network")
}

func TestStatusAllWithNothingTracked(t *testing.T) {
	// A network with no deployments is trivially in sync; config-declared
	// contracts that were never deployed there are not drift.
	h := newHarness(t, harnessOptions{})

	result, err := h.svc.StatusAll(context.Background(), "mainnet")
	require.NoError(t, err)
	assert.True(t, result.InSync)
	assert.Empty(t, result.Contracts)
	assert.NotNil(t, result.Contracts, "an empty list, not null, in JSON")
}

func TestStatusAllRejectsUnknownNetwork(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	_, err := h.svc.StatusAll(context.Background(), "nowhere")
	assert.Error(t, err)
}

func TestStatusSACInSync(t *testing.T) {
	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  xlm:
    asset: native
`})
	ctx := context.Background()

	sacID := otherContractID(t, 10)
	_, err := h.store.UpsertContract(ctx, store.Contract{
		Alias:           "xlm",
		Network:         "testnet",
		ContractID:      sacID,
		CurrentWasmHash: "stellar_asset",
	})
	require.NoError(t, err)

	require.NoError(t, h.client.SetSACInstance(sacID))

	status, err := h.svc.Status(ctx, deploy.StatusRequest{Alias: "xlm"})
	require.NoError(t, err)

	assert.Equal(t, deploy.StateInSync, status.State)
	assert.Equal(t, "stellar_asset", status.ExpectedWasmHash)
	assert.Equal(t, "stellar_asset", status.ActualWasmHash)
	assert.True(t, status.InSync())
}

func TestStatusSACDrift(t *testing.T) {
	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  xlm:
    asset: native
`})
	ctx := context.Background()

	sacID := otherContractID(t, 10)
	_, err := h.store.UpsertContract(ctx, store.Contract{
		Alias:           "xlm",
		Network:         "testnet",
		ContractID:      sacID,
		CurrentWasmHash: "some_old_wasm_hash",
	})
	require.NoError(t, err)

	require.NoError(t, h.client.SetSACInstance(sacID))

	status, err := h.svc.Status(ctx, deploy.StatusRequest{Alias: "xlm"})
	require.NoError(t, err)

	assert.Equal(t, deploy.StateDrift, status.State)
	assert.Equal(t, "stellar_asset", status.ActualWasmHash)
	assert.False(t, status.InSync())
}
