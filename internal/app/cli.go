// Package app wires the firefly-jar command-line entry point: argument parsing, subcommand dispatch and
// the process exit code.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/spf13/cobra"

	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/logging"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Exit codes of contracts/cli.md that the CLI itself decides; a check run's own 0, 1 or 2 comes from
// its report.
const (
	exitOK    = 0
	exitError = 2
)

// checkTimeout caps one whole check run, so a hung provider can never keep cron's job alive (R12).
const checkTimeout = 10 * time.Minute

// develVersion is reported when the binary carries no module version, as in tests and `go run`.
const develVersion = "(devel)"

// errNoCommand is the usage error of a bare `firefly-jar` with no subcommand.
var errNoCommand = errors.New("no command given")

// Env is everything a run takes from its process: the console streams, the environment, the
// clock, the base transport of every outgoing HTTP client (nil means http.DefaultTransport) and the
// Factory that builds a command's dependencies. Tests inject all of it, so a run never touches the
// real console, environment or network.
type Env struct {
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	Getenv    func(string) string
	Now       func() time.Time
	Transport http.RoundTripper
	Factory   Factory
}

// Factory builds the dependencies of one command from its validated input. BuildDeps is the
// production Factory.
type Factory func(ctx context.Context, in BuildInput) (Deps, error)

// Run is the CLI entry point invoked by main. Stdin, stdout and stderr are injected so callers, and
// tests, never touch the real console. It returns the process exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return RunEnv(args, Env{
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
		Getenv:  os.Getenv,
		Now:     time.Now,
		Factory: BuildDeps,
	})
}

// RunEnv parses args, runs the chosen command against env and returns the process exit code. A
// parse error (unknown command, flag or argument count) prints the error and the usage on stderr
// and exits 2; every other exit code is the command's own.
func RunEnv(args []string, env Env) int {
	c := newCLI(env)
	root := c.rootCommand()

	// A nil slice would make cobra fall back to os.Args, so the process arguments could leak in.
	root.SetArgs(append([]string{}, args...))
	root.SetIn(c.env.Stdin)
	root.SetOut(c.env.Stdout)
	root.SetErr(c.env.Stderr)

	cmd, err := root.ExecuteContextC(context.Background())
	if err != nil {
		fmt.Fprintf(c.env.Stderr, "firefly-jar: %v\n%s", err, cmd.UsageString())

		return exitError
	}

	return c.code
}

// version reports the module version stamped into the binary by the Go toolchain.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return develVersion
	}

	return info.Main.Version
}

// cli is one invocation: its environment, the parsed global flags and the exit code the command
// chose.
type cli struct {
	env        Env
	configPath string
	code       int
}

// newCLI fills the optional parts of env with the process defaults.
func newCLI(env Env) *cli {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}

	if env.Now == nil {
		env.Now = time.Now
	}

	if env.Factory == nil {
		env.Factory = BuildDeps
	}

	return &cli{env: env, code: exitOK}
}

// rootCommand builds the command tree. Cobra's own error and usage printing is silenced, so RunEnv
// alone decides what reaches stderr and with which exit code.
func (c *cli) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "firefly-jar",
		Short:         "Remind about bank transactions missing from Firefly III",
		Version:       version(),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return errNoCommand
		},
	}

	root.SetVersionTemplate("firefly-jar {{.Version}}\n")
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVar(&c.configPath, "config", "",
		"config file (default "+config.DefaultPath+", or $FIREFLY_JAR_CONFIG)")
	root.AddCommand(c.checkCommand(), c.authCommand(), c.accountsCommand())
	// Cobra adds these flags lazily on execution; adding them now keeps them in the usage printed
	// after a parse error on the root command.
	root.InitDefaultHelpFlag()
	root.InitDefaultVersionFlag()

	return root
}

// checkCommand builds `check [--stdout]`.
func (c *cli) checkCommand() *cobra.Command {
	var stdout bool

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Report bank transactions missing from Firefly III",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c.code = c.runCheck(cmd.Context(), stdout)

			return nil
		},
	}

	cmd.Flags().BoolVar(&stdout, "stdout", false, "print the digest instead of sending it")

	return cmd
}

// authCommand builds `auth <bank>`, which is not implemented yet (US2).
func (c *cli) authCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "auth <bank>",
		Short: "Authorize read-only access to a bank",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, _ []string) error {
			c.code = c.notImplemented("auth")

			return nil
		},
	}
}

// accountsCommand builds `accounts [--ids]`, which is not implemented yet (US3).
func (c *cli) accountsCommand() *cobra.Command {
	var ids bool

	cmd := &cobra.Command{
		Use:   "accounts",
		Short: "Show how bank accounts map to Firefly III accounts",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c.code = c.notImplemented("accounts")

			return nil
		},
	}

	cmd.Flags().BoolVar(&ids, "ids", false, "add the full identification hash of every account")

	return cmd
}

// runCheck validates everything check needs before building any client, then builds its
// dependencies through the Factory and runs it under the 10-minute cap. Warnings found while
// loading go to the run's logger once it exists.
func (c *cli) runCheck(ctx context.Context, stdout bool) int {
	in, warnings, err := c.prepare(config.CmdCheck, stdout)
	if err != nil {
		return c.fail(ctx, warnings, err)
	}

	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	deps, err := c.env.Factory(ctx, in)
	if err != nil {
		return c.fail(ctx, warnings, fmt.Errorf("build dependencies: %w", err))
	}

	if deps.Close != nil {
		defer deps.Close()
	}

	for _, w := range warnings {
		deps.Log.WarnContext(ctx, "configuration warning", "warning", w)
	}

	_, code := Check(ctx, deps, CheckOptions{Stdout: stdout})

	return code
}

// prepare loads the config, validates it for cmd (resolving only cmd's secrets) and loads the
// state, collecting every warning on the way (FR-030).
func (c *cli) prepare(cmd config.Command, stdout bool) (BuildInput, []string, error) {
	cfg, warnings, err := config.Load(config.ResolvePath(c.configPath, c.env.Getenv), c.env.Getenv)
	if err != nil {
		return BuildInput{}, warnings, fmt.Errorf("load: %w", err)
	}

	secrets, more, err := cfg.ValidateFor(cmd, stdout)
	warnings = append(warnings, more...)

	if err != nil {
		return BuildInput{}, warnings, fmt.Errorf("validate: %w", err)
	}

	st, more, err := state.Load(cfg.StateFile)
	warnings = append(warnings, more...)

	if err != nil {
		return BuildInput{}, warnings, fmt.Errorf("load state: %w", err)
	}

	return BuildInput{
		Command: cmd,
		Stdout:  stdout,
		Config:  cfg,
		Secrets: secrets,
		State:   st,
		Env:     c.env,
	}, warnings, nil
}

// fail reports an error found before the run's logger exists: the warnings go to stderr as WARN
// records through a redacting stderr-only logger, then the error as one line, and the run exits 2.
func (c *cli) fail(ctx context.Context, warnings []string, err error) int {
	log := logging.New(io.Discard, c.env.Stderr, slog.LevelWarn, redact.New())

	for _, w := range warnings {
		log.WarnContext(ctx, "configuration warning", "warning", w)
	}

	fmt.Fprintf(c.env.Stderr, "firefly-jar: %v\n", err)

	return exitError
}

// notImplemented reports a command that a later user story delivers.
func (c *cli) notImplemented(name string) int {
	fmt.Fprintf(c.env.Stderr, "firefly-jar: %s: not implemented\n", name)

	return exitError
}
