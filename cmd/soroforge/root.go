package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/spf13/cobra"
)

// globalFlags are the options every subcommand shares.
type globalFlags struct {
	configPath string
	network    string
	logLevel   string
	logFormat  string
	jsonOutput bool
}

// Log output formats accepted by --log-format.
const (
	logFormatText = "text"
	logFormatJSON = "json"
)

var flags globalFlags

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "soroforge",
		Short: "Deploy, upgrade, and track Soroban smart contracts",
		Long: `SoroForge turns Soroban contract deployment into a tracked, repeatable workflow.

Every deploy and upgrade is recorded in Postgres with the contract ID, WASM hash,
network, deployer address, and transaction — so you can answer which version of
which contract is live on which network, and who put it there.

Configuration lives in soroforge.yaml. Secrets come from the environment:

  DATABASE_URL             Postgres connection string
  SOROFORGE_SECRET_KEY     deployer secret seed (S...), or
  SOROFORGE_KEYSTORE_PATH  path to a file containing the seed

The secret key is never logged, stored, or printed. See the README security notes.`,
		SilenceUsage: true,
		// Errors are printed once here rather than by both cobra and main.
		SilenceErrors: false,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return setupLogging(os.Stderr)
		},
	}

	cmd.PersistentFlags().StringVarP(&flags.configPath, "config", "c", "",
		"path to soroforge.yaml (default: ./soroforge.yaml or $SOROFORGE_CONFIG)")
	cmd.PersistentFlags().StringVarP(&flags.network, "network", "n", "",
		"network to target (default: the config's default_network)")
	cmd.PersistentFlags().StringVar(&flags.logLevel, "log-level", "info",
		"log verbosity: debug, info, warn, or error")
	cmd.PersistentFlags().StringVar(&flags.logFormat, "log-format", "",
		"log output format: text or json (default: text or $SOROFORGE_LOG_FORMAT)")
	cmd.PersistentFlags().BoolVar(&flags.jsonOutput, "json", false,
		"emit machine-readable JSON instead of human-readable text")

	cmd.AddCommand(
		newInitCmd(),
		newDeployCmd(),
		newUpgradeCmd(),
		newListCmd(),
		newHistoryCmd(),
		newStatusCmd(),
		newServeCmd(),
		newMigrateCmd(),
		newVersionCmd(),
	)

	return cmd
}

// setupLogging configures slog.
//
// Logs go to stderr so that --json output on stdout stays parseable by a pipe.
// The handler is text by default and becomes JSON when --log-format or
// SOROFORGE_LOG_FORMAT asks for it: JSON lines are what CI and log aggregators
// (Loki, CloudWatch, Datadog) index by field instead of regex.
func setupLogging(w io.Writer) error {
	level, err := parseLogLevel(flags.logLevel)
	if err != nil {
		return err
	}

	format := strings.ToLower(strings.TrimSpace(flags.logFormat))
	if format == "" {
		format = loadEnvOnly().LogFormat
	}
	if format == "" {
		format = logFormatText
	}

	var handler slog.Handler
	switch format {
	case logFormatText:
		handler = slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	case logFormatJSON:
		handler = slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	default:
		return fmt.Errorf("invalid --log-format %q: expected %s or %s", format, logFormatText, logFormatJSON)
	}

	slog.SetDefault(slog.New(handler))
	return nil
}

// parseLogLevel maps the flag's name to a slog level.
func parseLogLevel(name string) (slog.Level, error) {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid --log-level %q: expected debug, info, warn, or error", name)
	}
}

// app bundles everything a command needs, along with the cleanup it owns.
type app struct {
	cfg     *config.Config
	store   store.Store
	service *deploy.Service

	closers []func() error
}

// Close releases resources in reverse order of acquisition.
func (a *app) Close() error {
	var errs []error
	for i := len(a.closers) - 1; i >= 0; i-- {
		if err := a.closers[i](); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}

// appOptions selects which dependencies a command needs.
type appOptions struct {
	// requireSigner makes a missing signing key a startup error. Read-only
	// commands leave it false so that `list`, `history`, and `status` work
	// without any key present at all — there is no reason to expose one to run
	// a report.
	requireSigner bool

	// storeOptional allows the command to run without Postgres, using a
	// throwaway in-memory store. Only `deploy --dry-run` sets it: a dry run
	// records nothing, so requiring a database to validate a config would be
	// friction with no purpose. Every command that writes history still
	// insists on a real database.
	storeOptional bool
}

// newApp loads configuration, connects to Postgres, and builds the service.
func newApp(ctx context.Context, opts appOptions) (*app, error) {
	cfg, err := config.Load(flags.configPath)
	if err != nil {
		return nil, err
	}

	a := &app{cfg: cfg}

	if opts.storeOptional && cfg.Env.DatabaseURL == "" {
		slog.Debug("no DATABASE_URL set; using an in-memory store for this dry run")
		a.store = store.NewMemoryStore()
	} else {
		databaseURL, err := cfg.Env.RequireDatabaseURL()
		if err != nil {
			return nil, err
		}

		pg, err := store.NewPostgres(ctx, databaseURL)
		if err != nil {
			return nil, err
		}
		a.store = pg
		a.closers = append(a.closers, pg.Close)
	}

	signer, err := buildSigner(cfg, opts.requireSigner)
	if err != nil {
		_ = a.Close()
		return nil, err
	}

	service, err := deploy.New(deploy.Options{
		Config: cfg,
		Store:  a.store,
		Signer: signer,
		Log:    slog.Default(),
	})
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	a.service = service

	return a, nil
}

// loadEnvOnly reads SoroForge's environment without requiring soroforge.yaml.
// Migrations need only DATABASE_URL, and should work before a project config
// exists.
func loadEnvOnly() config.Env { return config.LoadEnv() }

// buildSigner reads the deployer key, if one is configured.
//
// The seed goes straight from the environment into the signer and is never held
// anywhere else. Only the derived public address is logged, and only at debug
// level, so that operators can confirm which key was used without the value
// appearing anywhere.
func buildSigner(cfg *config.Config, required bool) (stellar.Signer, error) {
	if !cfg.Env.HasSigningKey() {
		if required {
			return nil, fmt.Errorf(
				"no signing key configured: set %s or %s (see README security notes)",
				config.EnvSecretKey, config.EnvKeystorePath)
		}
		return nil, nil
	}

	seed, source, err := cfg.Env.SigningSeed()
	if err != nil {
		return nil, err
	}

	signer, err := stellar.NewKeypairSigner(seed)
	if err != nil {
		return nil, fmt.Errorf("%w (key source: %s)", err, source)
	}

	slog.Debug("loaded signing key", "source", source, "address", signer.Address())
	return signer, nil
}
