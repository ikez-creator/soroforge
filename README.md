# SoroForge

[![CI](https://github.com/soroworks/soroforge/actions/workflows/ci.yml/badge.svg)](https://github.com/soroworks/soroforge/actions/workflows/ci.yml)
[![Security](https://github.com/soroworks/soroforge/actions/workflows/security.yml/badge.svg)](https://github.com/soroworks/soroforge/actions/workflows/security.yml)
[![Release](https://img.shields.io/github/v/release/soroworks/soroforge)](https://github.com/soroworks/soroforge/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/soroworks/soroforge)](go.mod)
[![License](https://img.shields.io/github/license/soroworks/soroforge)](LICENSE)

Deployment and lifecycle management for Stellar/Soroban smart contracts.

SoroForge turns contract deployment into a tracked, repeatable workflow with a
recorded history — so you manage contracts the way you manage any other deployed
software.

> **Independent project.** SoroForge is an independent, community-built tool. It
> is not affiliated with, endorsed by, or supported by the Stellar Development
> Foundation. "Stellar" and "Soroban" are used only to describe what this tool
> works with. For official tooling, see the
> [Stellar CLI](https://developers.stellar.org/docs/tools/developer-tools).

---

## Why

Deploying Soroban contracts today is a pile of one-off CLI invocations with no
memory. A week later, nobody can answer:

- Which WASM hash is live on testnet? On mainnet? Are they the same build?
- What changed between the version that's live and the one in `main`?
- Who deployed it, when, and which transaction did it?
- Has anyone changed it since?

The answers live in someone's shell history, if they live anywhere. SoroForge
records every deploy and upgrade in Postgres — contract ID, WASM hash, network,
deployer address, transaction, ledger, and free-form notes — and can verify that
record against the chain on demand.

That last part matters most. A deployment tracker that never checks itself will
confidently report stale information. `soroforge status` compares the WASM hash
the contract is *actually* running against the one SoroForge recorded, and tells
you when they disagree.

---

## Verified SDK and RPC versions

Soroban's deployment flow and RPC surface change. Everything below was verified
against the live API rather than recalled, and the live smoke tests
(`make test-live`) exist so you can re-verify after an upgrade.

| Component | Version | Notes |
|---|---|---|
| [`github.com/stellar/go-stellar-sdk`](https://github.com/stellar/go-stellar-sdk) | **v0.7.1** | The SDK repository was renamed from `github.com/stellar/go` in the October 2025 refactor. The old path is not used. |
| `clients/rpcclient` | (in SDK v0.7.1) | The Soroban RPC client now ships inside the SDK. No separate `stellar-rpc` client module is needed. |
| `protocols/rpc` | (in SDK v0.7.1) | Request/response types, imported as `protocol`. |
| Stellar RPC | Protocol 23 era | Field names verified against the SDK source; several differ from older tutorials. |
| Go | 1.25 | Required by the SDK's own `go.mod`. |

Two details that trip up code written from older examples:

- **Field names changed.** The simulation response uses `TransactionDataXDR`,
  `MinResourceFee`, `Results[].ReturnValueXDR`, and `Results[].AuthXDR`;
  resources use `DiskReadBytes` (formerly `ReadBytes`).
- **There is no `AssembleTransaction` helper in the Go SDK**, unlike the
  JavaScript SDK. SoroForge assembles transactions itself in
  [`internal/stellar/assemble.go`](internal/stellar/assemble.go): it decodes the
  simulated `SorobanTransactionData`, attaches it and the recorded auth entries
  to the operation, and rebuilds the transaction so that `txnbuild` recomputes
  the fee as `BaseFee × operations + ResourceFee`.

Other pinned dependencies: `pgx/v5 v5.10.0`, `golang-migrate/v4 v4.19.1`,
`cobra v1.10.2`, `chi/v5 v5.3.1`, `testify v1.11.1`, `yaml.v3 v3.0.1`.

---

## How deploying works

Worth knowing, because it explains the command output.

**Deploy is two transactions.** Soroban permits one host function per
transaction, so SoroForge first uploads the bytecode — which becomes addressable
by its SHA-256 hash — and then creates a contract instance from that hash. The
instance gets an address; the bytecode is shared by every contract using it. If
the bytecode is already on-chain, the upload is skipped.

**Upgrade is not a protocol operation.** Soroban has no "upgrade" instruction. A
contract upgrades *itself* through a function it chooses to expose, typically:

```rust
pub fn upgrade(env: Env, new_wasm_hash: BytesN<32>) {
    let admin: Address = env.storage().instance().get(&DataKey::Admin).unwrap();
    admin.require_auth();
    env.deployer().update_current_contract_wasm(new_wasm_hash);
}
```

SoroForge uploads the new bytecode and invokes that function. A contract that
doesn't expose one **cannot be upgraded**, and SoroForge will report the
contract's own error. The entrypoint name defaults to `upgrade` and is
configurable per contract via `upgrade_fn`.

---

## Quickstart — deploy to testnet

**Prerequisites:** Go 1.25+, Docker (for Postgres), a compiled contract `.wasm`,
and a funded testnet account.

```bash
git clone https://github.com/soroworks/soroforge.git
cd soroforge
make build
```

**1. Start Postgres and create the schema.**

```bash
docker compose up -d
make migrate-up
```

**2. Configure your project.** From your contract project, after
`stellar contract build`:

```bash
soroforge init
```

`init` finds the release WASM under `target/wasm32v1-none/release` (or the
older `wasm32-unknown-unknown`), preferring a `.optimized.wasm`, and writes a
`soroforge.yaml` with one alias per contract, targeting testnet:

```yaml
contracts:
  counter:
    wasm: "./target/wasm32v1-none/release/counter.wasm"
```

It never replaces an existing file without `--force`.

**3. Provide credentials.**

```bash
cp .env.example .env
```

Set `DATABASE_URL`, and a deployer key via either `SOROFORGE_SECRET_KEY` or
`SOROFORGE_KEYSTORE_PATH`. Fund the account at
<https://friendbot.stellar.org> if you haven't. Read the [security
notes](#security-notes-on-key-handling) first.

**4. Dry run.** This assembles and simulates both transactions, prints the
contract address a real deploy would produce, and submits and records nothing:

```bash
./bin/soroforge deploy counter --network testnet --dry-run
```

A dry run doesn't need `DATABASE_URL`, so it doubles as a config check.

**5. Deploy for real.**

```bash
./bin/soroforge deploy counter --network testnet --notes "v1.0.0"
```

**6. Confirm it's tracked, and that the record is true.**

```bash
./bin/soroforge list
./bin/soroforge history counter
./bin/soroforge status counter
```

---

## Configuration reference

`soroforge.yaml` holds what's shareable and belongs in version control. Secrets
come from the environment and never go in this file. Unknown keys are rejected,
so a typo is a loud error rather than a silently ignored setting.

```yaml
version: 1                  # schema version; only 1 exists
default_network: testnet    # used when --network is omitted

networks:
  testnet:
    rpc_url: https://soroban-testnet.stellar.org
    passphrase: "Test SDF Network ; September 2015"
    sorovault_url: http://localhost:8080   # optional

contracts:
  counter:
    wasm: ./target/wasm32-unknown-unknown/release/counter.wasm
    constructor_args: []    # optional
    upgrade_fn: upgrade     # optional, defaults to "upgrade"
    salt: ""                # optional 32-byte hex
```

**Networks.** Any number, named freely. The passphrase is required rather than
inferred from the name — it's mixed into every signature, so a wrong value
produces a transaction that's invalid on the network you meant to use, and a
silently defaulted one could sign a mainnet deploy with testnet assumptions.

There is **no public SDF-hosted mainnet RPC**. Point `mainnet` at a provider you
trust or at your own node.

**`sorovault_url`** registers every confirmed deploy and upgrade with a
[SoroVault](https://github.com/soroworks/sorovault) registry serving that
network, so the contract's functions, types and events are discoverable the
moment it is live — and [SoroProbe](https://github.com/soroworks/soroprobe)
can type its arguments from them. An upgrade records a new interface version
and keeps the old one. Registration happens only after on-chain confirmation
and is best-effort: if SoroVault is down, the deploy still succeeds and is
recorded, and the output (`catalog` in JSON) says registration failed so you
can run `sorovault add` later. Dry runs never register anything.

**Contracts.** `wasm` paths resolve against the config file's directory, not your
shell's working directory, so `soroforge deploy` behaves the same from anywhere.

**`salt`** makes a contract's address reproducible: addresses derive from
(deployer address, salt). Leave it unset and each deploy uses a random salt, so
deploying the same alias twice produces two distinct contracts.

### Constructor arguments

Soroban takes arguments as XDR `ScVal` values, which nobody wants to hand-write.
Write them typed instead:

```yaml
constructor_args:
  - {type: address, value: GABC...}
  - {type: u32, value: 7}
  - {type: string, value: "Example Token"}
  - {type: i128, value: "170141183460469231731687303715884105727"}
  - {type: vec, value: [{type: u32, value: 1}, {type: u32, value: 2}]}
  - {type: map, value: [{key: {type: symbol, value: decimals},
                         value: {type: u32, value: 7}}]}
```

| Type | Value form |
|---|---|
| `bool` | `true` / `false` |
| `void` | omit `value` |
| `u32` `i32` `u64` `i64` | decimal integer |
| `u128` `i128` | decimal integer **as a quoted string** |
| `string` `symbol` | text (`symbol` is `[a-zA-Z0-9_]`, max 32 chars) |
| `bytes` | hex, with or without `0x` |
| `address` | `G...` account or `C...` contract |
| `vec` | a list of args |
| `map` | a list of `{key, value}` pairs |
| `xdr` | base64 `ScVal` — the escape hatch |

Quote anything beyond float64's exact range, or YAML will round it before
SoroForge sees it. The `xdr` type accepts a pre-encoded `ScVal`, so a type not
listed here is never a hard block.

Adding a type means one case in
[`internal/config/args.go`](internal/config/args.go) and one table entry in its
test.

### Environment variables

| Variable | Purpose |
|---|---|
| `DATABASE_URL` | Postgres connection string. Required except for `deploy --dry-run`. |
| `SOROFORGE_SECRET_KEY` | Deployer secret seed (`S...`). |
| `SOROFORGE_KEYSTORE_PATH` | Path to a file containing only the seed. |
| `SOROFORGE_API_TOKEN` | Read-write bearer token for `serve`. |
| `SOROFORGE_API_TOKENS` | Scoped bearer tokens for `serve`, as `name:scope:token` entries — see below. |
| `SOROFORGE_API_ADDR` | Listen address (default `127.0.0.1:8080`). |
| `SOROFORGE_LOG_FORMAT` | `text` (default) or `json`; same as `--log-format`. |
| `SOROFORGE_CONFIG` | Config file path (default `./soroforge.yaml`). |
| `TEST_DATABASE_URL` | Enables Postgres integration tests. |
| `SOROFORGE_LIVE` | Enables the live testnet smoke tests. |

---

## CLI reference

Global flags: `--config/-c`, `--network/-n`, `--log-level`, `--log-format`,
`--json`. Output goes to stdout and logs to stderr, so `--json` output stays
pipeable.

`--log-format text|json` (default `text`, or `$SOROFORGE_LOG_FORMAT`) selects
the slog handler. JSON mode writes one JSON object per line, for CI and log
aggregators (Loki, CloudWatch, Datadog) that filter by field rather than
regex:

```bash
soroforge deploy counter --log-format json
{"time":"2026-01-02T15:04:05Z","level":"INFO","msg":"loaded signing key","source":"env:SOROFORGE_SECRET_KEY","address":"G..."}
```

An unknown format is rejected with the accepted values rather than silently
falling back.

| Command | Description |
|---|---|
| `init` | Write a starter `soroforge.yaml` for the contracts built under `target/`. |
| `deploy <alias>` | Upload WASM, instantiate the contract, record it. |
| `upgrade <alias>` | Upload new WASM and invoke the contract's upgrade entrypoint. |
| `list` | List tracked contracts; `--network` filters. |
| `history <alias>` | Show a contract's full deploy/upgrade timeline, newest first. |
| `status <alias>` | Compare the on-chain WASM hash against the recorded one. |
| `status --all` | Drift-check every tracked contract on the network, for a single CI gate. |
| `serve` | Run the HTTP API. |
| `migrate up\|down\|version` | Manage the schema. |
| `version` | Print the SoroForge version. |

`deploy` and `upgrade` accept `--dry-run` and `--notes`.

**`--dry-run`** runs everything up to and including simulation and assembly,
then stops. It prints the assembled envelopes and the resulting contract
address, and writes nothing and submits nothing. It's safe to point at mainnet.

**`status` exit codes** make it usable as a CI gate:

| Code | Meaning |
|---|---|
| `0` | In sync. |
| `1` | The check could not be completed. |
| `2` | Drift, untracked, or missing on-chain. |

The four states it reports:

- **`in_sync`** — the chain matches the record.
- **`drift`** — the contract is running bytecode SoroForge didn't deploy.
  Someone changed it another way.
- **`untracked`** — SoroForge has no record of this contract on this network.
- **`missing`** — SoroForge has a record, but the chain has no such contract.

---

## HTTP API reference

Mirrors the CLI, for CI pipelines that can't easily run a binary. Both surfaces
call the same service, so behaviour cannot drift between them.

```bash
soroforge serve
```

Every `/v1` endpoint requires `Authorization: Bearer <token>`. The server
**refuses to start without a token** — see the security notes.

### Scoped tokens

`SOROFORGE_API_TOKEN` is a read-write token, so existing single-token setups
keep working unchanged. `SOROFORGE_API_TOKENS` adds scoped tokens as a
comma-separated list of `name:scope:token` entries:

```
SOROFORGE_API_TOKEN=$(openssl rand -hex 32)             # read-write
SOROFORGE_API_TOKENS="dashboard:read:$DASHBOARD_TOKEN,ci:write:$CI_TOKEN"
```

- `read` tokens may call `GET` routes only. `write` tokens may call everything
  (write implies read).
- The name is a label for access logs (`"token":"dashboard"`), never the value.
- Tokens must be at least 16 characters, and names and values must be unique.
- A token is compared in constant time against every configured token, so
  timing cannot reveal which one matched or how far a guess got.

Use a read-only token for dashboards, monitoring jobs, and anything that only
reads state — it still cannot be used to sign a mainnet transaction if it leaks.
A valid token that lacks the required scope gets **403 Forbidden**, not 401:
the credential is real, the operation simply isn't permitted. A missing or
unknown token is **401 Unauthorized**.

| Method | Path | Scope | Description |
|---|---|---|---|
| `GET` | `/health` | public | Liveness. Process up; dependencies not checked. |
| `GET` | `/ready` | public | Readiness. `200` when the database answers and each configured network's RPC returns a latest ledger; `503` with the failing dependency names otherwise. Checks are cached briefly so probes cannot hammer the RPC. |
| `POST` | `/v1/deploy` | write | `{"alias","network","notes","dry_run"}` → `201` (`200` for a dry run). |
| `POST` | `/v1/upgrade` | write | Same body → `200`. |
| `GET` | `/v1/contracts?network=` | read | Tracked contracts. |
| `GET` | `/v1/contracts/{network}/{alias}/history` | read | Deployment history. |
| `GET` | `/v1/contracts/{network}/{alias}/status` | read | Drift check. |
| `GET` | `/v1/contracts/{network}/status` | read | Drift check of every tracked contract; `in_sync` is the verdict. |

Every route's scope is declared in one table in
[`internal/api/api.go`](internal/api/api.go), and a test walks the built router
to fail if a route is ever added without one.

```bash
curl -X POST http://localhost:8080/v1/deploy \
  -H "Authorization: Bearer $SOROFORGE_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"alias":"counter","network":"testnet","dry_run":true}'
```

Unknown JSON fields are rejected, so a misspelled `dry_run` fails loudly instead
of deploying for real. Failures return `{"error": "..."}`; drift returns `200`
with `"state":"drift"`, because the check ran and produced an answer.

Each request writes one structured access log line to stderr at `info` — method,
route pattern, status, duration, request ID, client address, and the token's
name:

```json
{"time":"2026-01-02T15:04:05Z","level":"INFO","msg":"http request","method":"POST","route":"/v1/deploy","status":201,"duration":1843000000,"request_id":"...","remote_addr":"127.0.0.1:52344","token":"ci"}
```

The bearer token value, request bodies, and key material are never logged.
`/health` is logged at `debug` only, so a liveness probe polling every second
does not flood the log.

---

## Security notes on key handling

**SoroForge reads a key you provide. It does not generate, store, rotate, or
manage keys** — that's deliberate, and out of scope.

**What SoroForge does with your key:**

- Reads the seed from `SOROFORGE_SECRET_KEY` or the file at
  `SOROFORGE_KEYSTORE_PATH`. If both are set, the environment variable wins, so
  CI can override a developer's on-disk key.
- Passes it straight into a signer, which holds it unexported.
- Records only the derived public `G...` address.

**What it never does:** log the seed at any level, write it to the database,
include it in `--json` output or API responses, or echo it in an error message —
a malformed seed is still a secret, and errors end up in CI logs.
`Env.Redacted()` reports every secret as `(set)`/`(unset)`.

**Recommendations:**

- Prefer a **keystore file** locally, at mode `0600`. Environment variables are
  readable by every child process and land in shell history.
- Use your CI provider's secret store for `SOROFORGE_SECRET_KEY`. Don't commit
  `.env` — it's gitignored, keep it that way.
- Use a **separate key per network**. A mainnet deploy key shouldn't be on a
  laptop that also deploys to testnet.
- Always `--dry-run` against mainnet first.

**HTTP API:** the server refuses to start without `SOROFORGE_API_TOKEN` or
`SOROFORGE_API_TOKENS`, and rejects tokens shorter than 16 characters. Tokens
are compared in constant time, since a naive comparison leaks how much of the
token matched through response timing. It binds `127.0.0.1` by default because
this process holds a signing key; widen that only behind your own TLS and
authentication. Generate a real token with `openssl rand -hex 32`.

Prefer a **read-only token** (`name:read:...`) for anything that does not
deploy — a dashboard, a monitoring job, a status page. It can call every `GET`
route but is refused with `403` on `/v1/deploy` and `/v1/upgrade`, so a leak
cannot become a mainnet transaction.

**Not implemented, by design:** key generation, multisig or threshold signing,
hardware wallets, and CI plugin integrations. The `stellar.Signer` interface is
the extension point for the signing-related ones — see
[`internal/stellar/signer.go`](internal/stellar/signer.go).

---

## Architecture

```
cmd/soroforge/      cobra CLI
internal/config/    soroforge.yaml + env loading, validation, ScVal encoding
internal/stellar/   RPC client, transaction assembly, signing (all interfaced)
internal/deploy/    deploy/upgrade/status orchestration
internal/store/     Postgres history + embedded migrations
internal/api/       chi HTTP handlers mirroring the CLI
internal/readiness/ database + per-network RPC probes behind GET /ready
internal/catalog/   best-effort registration with SoroVault after a deploy
```

Three interfaces are the seams: `stellar.Client` (RPC), `stellar.Signer`
(signing), and `store.Store` (persistence). Each has a production implementation
and an in-memory one, which is why `go test ./...` needs neither a network nor a
database.

`stellar.Client` deliberately speaks base64 XDR strings rather than SDK structs,
so a fake needs no XDR plumbing. Everything that interprets XDR — building
operations, assembling simulated transactions, deriving contract IDs — is a pure
function, tested directly.

### Data model

`contracts` holds current state, one row per contract per network, unique on
`(network, alias)`. `deployments` is append-only: an upgrade adds a row rather
than modifying one, so the rows for a contract *are* its version history. Both
are indexed on `(network, contract_id)`.

Records are written **only after on-chain confirmation**. A history claiming a
deploy that never landed is worse than no history.

---

## Testing

```bash
make test          # no network, no database
make test-race
make test-integration   # adds the Postgres suite (needs docker compose up -d)
make test-live          # read-only smoke tests against live testnet
```

`go test ./...` never requires a network or a database — the Postgres and live
tests skip unless their environment variable is set. The Postgres suite runs the
*same* test suite as the in-memory store, which keeps the fake honest about
matching real database semantics.

The live tests are read-only (no keys, no submissions, no fees). They exist
because the rest of the suite proves SoroForge is internally consistent, not
that its understanding of the RPC API is still correct. Run them after upgrading
the Stellar SDK.

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The interfaces above are the intended
extension points, and `TODO(extension)` comments mark where new implementations
plug in.

## License

[Apache-2.0](LICENSE).
