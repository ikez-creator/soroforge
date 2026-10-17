package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/soroworks/soroforge/internal/api"
	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/readiness"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/spf13/cobra"
)

// version is set at build time via -ldflags "-X main.version=..."; see the Makefile.
var version = "dev"

func newDeployCmd() *cobra.Command {
	var (
		dryRun bool
		notes  string
	)

	cmd := &cobra.Command{
		Use:   "deploy <alias>",
		Short: "Deploy a contract and record it",
		Long: `Upload a contract's WASM and instantiate it on the target network.

This is two transactions: the bytecode is uploaded and becomes addressable by
its hash, then a contract instance is created from that hash. Both must confirm
before anything is recorded.

Use --dry-run to assemble and simulate the transactions without submitting
them. A dry run prints the contract address a real deploy would produce and the
assembled envelopes, and writes nothing to the database.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// A dry run writes nothing, so it does not need Postgres — that
			// makes it usable as a config check in a fresh checkout.
			a, err := newApp(cmd.Context(), appOptions{
				requireSigner: true,
				storeOptional: dryRun,
			})
			if err != nil {
				return err
			}
			defer a.Close()

			result, err := a.service.Deploy(cmd.Context(), deploy.DeployRequest{
				Alias:   args[0],
				Network: flags.network,
				Notes:   notes,
				DryRun:  dryRun,
			})
			if err != nil {
				return err
			}
			return printDeployResult(result)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"assemble and simulate without submitting or recording anything")
	cmd.Flags().StringVar(&notes, "notes", "",
		"free-form note recorded with this deployment (e.g. a ticket or release tag)")

	return cmd
}

func newUpgradeCmd() *cobra.Command {
	var (
		dryRun bool
		notes  string
	)

	cmd := &cobra.Command{
		Use:   "upgrade <alias>",
		Short: "Upgrade a tracked contract to new bytecode",
		Long: `Replace a tracked contract's WASM, keeping its address.

Soroban has no protocol-level upgrade operation. Upgrading is something a
contract does to itself, so this uploads the new bytecode and then invokes the
contract's own upgrade entrypoint — "upgrade" by default, or whatever
'upgrade_fn' names in soroforge.yaml. A contract that does not expose such a
function, or that rejects the caller, will fail here with its own error.

The current hash is read from the ledger rather than the database, so an
upgrade applied outside SoroForge is still reported accurately.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(cmd.Context(), appOptions{requireSigner: true})
			if err != nil {
				return err
			}
			defer a.Close()

			result, err := a.service.Upgrade(cmd.Context(), deploy.UpgradeRequest{
				Alias:   args[0],
				Network: flags.network,
				Notes:   notes,
				DryRun:  dryRun,
			})
			if err != nil {
				return err
			}
			return printUpgradeResult(result)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"assemble and simulate without submitting or recording anything")
	cmd.Flags().StringVar(&notes, "notes", "",
		"free-form note recorded with this upgrade")

	return cmd
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List tracked contracts",
		Long: `Show every contract SoroForge tracks, across all networks.

Pass --network to restrict the list to one network.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := newApp(cmd.Context(), appOptions{})
			if err != nil {
				return err
			}
			defer a.Close()

			contracts, err := a.service.List(cmd.Context(), flags.network)
			if err != nil {
				return err
			}
			return printContracts(contracts)
		},
	}
}

func newHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "history <alias>",
		Short: "Show a contract's deploy and upgrade history",
		Long: `Show every recorded change to one contract on one network, newest first.

Each entry records the WASM hash, who deployed it, the transaction, and the
ledger it landed in.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(cmd.Context(), appOptions{})
			if err != nil {
				return err
			}
			defer a.Close()

			alias := args[0]
			network, _, err := a.cfg.Network(flags.network)
			if err != nil {
				return err
			}

			deployments, err := a.service.History(cmd.Context(), network, alias)
			if err != nil {
				return err
			}
			return printHistory(alias, network, deployments)
		},
	}
}

func newStatusCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "status [alias]",
		Short: "Check whether contracts match their recorded state",
		Long: `Compare the WASM hash on-chain against the hash SoroForge recorded.

The ledger is the authority: if they disagree, the contract was changed outside
SoroForge and the command reports drift.

With --all, every contract SoroForge tracks on the network is checked —
including any whose alias has since been removed from soroforge.yaml — so a
single CI step can gate on all of them.

Exit codes:
  0  in sync (with --all: every contract)
  1  the check could not be completed
  2  drift, untracked, or missing on-chain (with --all: any contract)

The non-zero exit on drift makes this usable as a CI gate.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if all {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(cmd.Context(), appOptions{})
			if err != nil {
				return err
			}
			defer a.Close()

			if all {
				result, err := a.service.StatusAll(cmd.Context(), flags.network)
				if err != nil {
					return err
				}
				if err := printNetworkStatus(result); err != nil {
					return err
				}
				if !result.InSync {
					cmd.SilenceErrors = true
					return &driftError{detail: fmt.Sprintf("contracts on %s are out of sync", result.Network)}
				}
				return nil
			}

			result, err := a.service.Status(cmd.Context(), deploy.StatusRequest{
				Alias:   args[0],
				Network: flags.network,
			})
			if err != nil {
				return err
			}
			if err := printStatus(result); err != nil {
				return err
			}

			if !result.InSync() {
				// Printed already; this only selects the exit code.
				cmd.SilenceErrors = true
				return &driftError{detail: result.Detail}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "check every tracked contract on the network")
	return cmd
}

func newServeCmd() *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP API for CI pipelines",
		Long: `Serve the same operations as the CLI over HTTP.

Every /v1 endpoint requires a bearer token; the server refuses to start
without one, because it can deploy contracts using the configured signing
key. SOROFORGE_API_TOKEN is a read-write token. SOROFORGE_API_TOKENS adds
scoped tokens as name:scope:token entries, where scope is read or write —
use read for dashboards and monitoring, which can then only call GET routes.
It binds 127.0.0.1 by default — override with --addr only when you have put
authentication and TLS in front of it.

Endpoints:
  GET  /health   liveness, no auth
  GET  /ready    readiness (database and each network's RPC), no auth
  POST /v1/deploy
  POST /v1/upgrade
  GET  /v1/contracts?network=<name>
  GET  /v1/contracts/{network}/{alias}/history
  GET  /v1/contracts/{network}/{alias}/status
  GET  /v1/contracts/{network}/status`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := newApp(cmd.Context(), appOptions{})
			if err != nil {
				return err
			}
			defer a.Close()

			tokens, err := api.ParseTokens(a.cfg.Env.APIToken, a.cfg.Env.APITokens)
			if err != nil {
				return err
			}

			readinessChecker, err := readiness.New(readiness.Options{
				Store:   a.store,
				Config:  a.cfg,
				Clients: deploy.DefaultClientFactory,
				Log:     slog.Default(),
			})
			if err != nil {
				return err
			}

			listenAddr := addr
			if listenAddr == "" {
				listenAddr = a.cfg.Env.ListenAddr()
			}

			handler, err := api.NewRouter(api.Options{
				Service:   a.service,
				Tokens:    tokens,
				Readiness: readinessChecker,
				Log:       slog.Default(),
			})
			if err != nil {
				return err
			}

			return serve(cmd.Context(), api.NewServer(listenAddr, handler))
		},
	}

	cmd.Flags().StringVar(&addr, "addr", "",
		"listen address (default: $SOROFORGE_API_ADDR or 127.0.0.1:8080)")

	return cmd
}

// serve runs the HTTP server until interrupted, then shuts it down gracefully
// so an in-flight deploy is not cut off mid-submission.
func serve(ctx context.Context, srv *http.Server) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("HTTP API listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down, waiting for in-flight requests")

		// Long enough for a deploy that is already waiting on confirmation to
		// finish rather than being abandoned half-recorded.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Manage the deployment history schema",
		Args:  cobra.NoArgs,
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "up",
			Short: "Apply pending migrations",
			Long: `Apply all pending migrations.

Running this against an already-current schema is a no-op, so it is safe to run
on every start.`,
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return withMigrator(func(m *store.Migrator) error {
					if err := m.Up(); err != nil {
						return err
					}
					version, dirty, err := m.Version()
					if err != nil {
						return err
					}
					fmt.Fprintf(out(), "Schema is at version %d (dirty: %t).\n", version, dirty)
					return nil
				})
			},
		},
		&cobra.Command{
			Use:   "down",
			Short: "Revert the most recent migration",
			Long: `Revert one migration.

This steps back a single migration rather than tearing the schema down, but it
can still destroy deployment history. Take a backup first.`,
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return withMigrator(func(m *store.Migrator) error {
					if err := m.Down(); err != nil {
						return err
					}
					version, dirty, err := m.Version()
					if err != nil {
						return err
					}
					fmt.Fprintf(out(), "Schema is at version %d (dirty: %t).\n", version, dirty)
					return nil
				})
			},
		},
		&cobra.Command{
			Use:   "version",
			Short: "Show the current schema version",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return withMigrator(func(m *store.Migrator) error {
					version, dirty, err := m.Version()
					if err != nil {
						return err
					}
					if dirty {
						fmt.Fprintf(out(),
							"Schema is at version %d and DIRTY — a migration failed partway "+
								"and needs manual repair.\n", version)
						return nil
					}
					fmt.Fprintf(out(), "Schema is at version %d.\n", version)
					return nil
				})
			},
		},
	)

	return cmd
}

// withMigrator runs fn against a migrator built from DATABASE_URL.
//
// Migrations only need the database, not the full application, so this avoids
// loading soroforge.yaml — `migrate up` should work before a project config
// exists.
func withMigrator(fn func(*store.Migrator) error) error {
	databaseURL, err := loadEnvOnly().RequireDatabaseURL()
	if err != nil {
		return err
	}

	migrator, err := store.NewMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := migrator.Close(); err != nil {
			slog.Warn("closing migrator", "error", err)
		}
	}()

	return fn(migrator)
}

func newInitCmd() *cobra.Command {
	var dir string
	var force bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a starter soroforge.yaml for the contracts in this project",
		Long: `Write a starter soroforge.yaml.

init looks for release WASM under target/wasm32v1-none/release (current Soroban
toolchains) and target/wasm32-unknown-unknown/release (older ones), preferring
a .optimized.wasm when there is one, and declares one contract per artifact,
named after its file. With nothing built yet, it writes a placeholder to edit.

The file targets testnet, needs no secrets, and is meant to be committed. An
existing file is never replaced without --force.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := flags.configPath
			if path == "" {
				path = filepath.Join(dir, config.DefaultFilename)
			}

			found, err := config.WriteScaffold(dir, path, force)
			if err != nil {
				return err
			}

			w := out()
			fmt.Fprintf(w, "Wrote %s.\n\n", path)
			if len(found) == 0 {
				fmt.Fprintln(w, "No built contracts found under target/; edit the placeholder after `stellar contract build`.")
			} else {
				for _, f := range found {
					fmt.Fprintf(w, "  %-20s %s\n", f.Alias, f.Wasm)
				}
			}
			fmt.Fprintln(w, "\nNext: soroforge deploy <alias> --dry-run")
			return nil
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".", "project directory to scan for built contracts")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing soroforge.yaml")
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the SoroForge version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(out(), "soroforge %s\n", version)
		},
	}
}
