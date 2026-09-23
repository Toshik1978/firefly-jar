package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/Toshik1978/firefly-jar/internal/bank/enablebanking"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
	"github.com/Toshik1978/firefly-jar/internal/httpx"
	"github.com/Toshik1978/firefly-jar/internal/logging"
	"github.com/Toshik1978/firefly-jar/internal/notify"
	"github.com/Toshik1978/firefly-jar/internal/notify/email"
	"github.com/Toshik1978/firefly-jar/internal/notify/telegram"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// authProvider names the bank-data provider whose Authorizer auth drives. Enable Banking is the only
// provider today, and the session it creates records the same name (contracts/state.md).
const authProvider = "enablebanking"

// BuildInput is what a Factory builds one command's dependencies from: the command and its
// --stdout flag, the loaded config, the secrets ValidateFor resolved for that command (and only
// those), the loaded state, and the run's environment.
type BuildInput struct {
	Command config.Command
	Stdout  bool
	Config  *config.Config
	Secrets config.Secrets
	State   *state.State
	Env     Env
}

// loadState reads the state file the config names into in.State and returns the warnings it raised.
func (in *BuildInput) loadState() ([]string, error) {
	st, warnings, err := state.Load(in.Config.StateFile)
	if err != nil {
		return warnings, fmt.Errorf("load state: %w", err)
	}

	in.State = st

	return warnings, nil
}

// BuildDeps is the production Factory. It builds per command (contracts/config.md): the Redactor
// from the resolved secrets and the state's session ids; the logger, which writes the JSON log file
// only for check; the Enable Banking client for every command; the Authorizer only for auth; the
// Firefly III client for accounts and check; and the notifiers only for check without --stdout, one
// per configured channel. The returned Deps.Close releases the log file.
func BuildDeps(_ context.Context, in BuildInput) (Deps, error) {
	redactor := newRedactor(in.Secrets, in.State)

	log, closeLog, err := newLogger(in, redactor)
	if err != nil {
		return Deps{}, err
	}

	eb := newEnableBanking(in, redactor)

	deps := Deps{
		Config:   in.Config,
		State:    in.State,
		Provider: eb,
		Redactor: redactor,
		Log:      log,
		Now:      in.Env.Now,
		Stdout:   in.Env.Stdout,
		Stderr:   in.Env.Stderr,
		Close:    closeLog,
	}

	if in.Command == config.CmdAuth {
		if err = setAuthorizer(&deps, authProvider, eb, in.Config.EnableBanking); err != nil {
			closeLog()

			return Deps{}, err
		}
	}

	if in.Command != config.CmdAuth {
		// firefly.New composes its own read-only and retry layers over the base transport. The run's
		// logger goes in so a skipped split's DEBUG record reaches log_file (research R8).
		deps.Firefly = firefly.New(in.Config.Firefly.URL, in.Secrets.FireflyToken, in.Env.Transport,
			firefly.WithLogger(log))
	}

	if in.Command == config.CmdCheck && !in.Stdout {
		deps.Notifiers = newNotifiers(in)
	}

	return deps, nil
}

// newRedactor scrubs every resolved secret and every stored session id, so a provider error that
// echoes a session id never reaches a log or a digest. A secret the command did not resolve was
// never read, so it cannot leak and needs no entry.
func newRedactor(secrets config.Secrets, st *state.State) *redact.Redactor {
	values := []string{secrets.FireflyToken, secrets.TelegramToken, secrets.SMTPPassword}

	if st != nil {
		for _, session := range st.Sessions {
			values = append(values, session.SessionID)
		}
	}

	return redact.New(values...)
}

// newLogger builds the command's logger and the function that releases it. Only check opens
// log_file, appending JSON records at log_level; auth and accounts log WARN and above to stderr
// only (contracts/config.md).
func newLogger(in BuildInput, r *redact.Redactor) (*slog.Logger, func(), error) {
	if in.Command != config.CmdCheck {
		return logging.New(io.Discard, in.Env.Stderr, slog.LevelWarn, r), func() {}, nil
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(in.Config.LogLevel)); err != nil {
		return nil, nil, fmt.Errorf("log_level: %w", err)
	}

	f, err := logging.OpenFile(in.Config.LogFile)
	if err != nil {
		return nil, nil, fmt.Errorf("log_file: %w", err)
	}

	return logging.New(f, in.Env.Stderr, level, r), func() { _ = f.Close() }, nil
}

// newEnableBanking builds the Enable Banking client over a retrying HTTP client whose every attempt
// is time-bounded (httpx.RetryTransport). The base URL is the production API root unless a test set
// Env.EnableBankingBaseURL, which stays empty outside a test (Env doc comment) so production always
// calls enablebanking.DefaultBaseURL.
func newEnableBanking(in BuildInput, r *redact.Redactor) *enablebanking.Client {
	hc := httpx.NewClient(&httpx.RetryTransport{Base: in.Env.Transport})
	signer := enablebanking.NewSigner(in.Config.EnableBanking.AppID, in.Secrets.PrivateKey, in.Env.Now)

	baseURL := enablebanking.DefaultBaseURL
	if in.Env.EnableBankingBaseURL != "" {
		baseURL = in.Env.EnableBankingBaseURL
	}

	return enablebanking.New(baseURL, hc, signer, r)
}

// setAuthorizer picks the Authorizer implementation by provider name. The Enable Banking client
// gets the redirect URL and PSU type only here, because Begin needs them and the fixed
// bank.Authorizer signature has no room for them.
func setAuthorizer(deps *Deps, provider string, eb *enablebanking.Client, cfg config.EnableBanking) error {
	switch provider {
	case authProvider:
		deps.Authorizer = eb.WithAuthConfig(cfg.RedirectURL, cfg.PSUType)

		return nil
	default:
		return fmt.Errorf("unknown bank provider %q", provider)
	}
}

// newNotifiers builds one notifier per configured channel. Telegram gets a client whose every
// attempt is time-bounded (httpx.TimeoutTransport) but without the shared retry layer, since it
// handles its own 429 back-off (R12); the retry layer would never retry its POSTs anyway.
// TelegramBaseURL and the email seams below are always their zero value outside a test (Env doc
// comment), so production always posts to the public Bot API and dials the configured SMTP host
// and port.
func newNotifiers(in BuildInput) []notify.Notifier {
	var notifiers []notify.Notifier

	if tg := in.Config.Notify.Telegram; tg != nil {
		hc := httpx.NewClient(&httpx.TimeoutTransport{Base: in.Env.Transport})
		notifiers = append(notifiers, telegram.New(in.Secrets.TelegramToken, tg.ChatIDs, hc, in.Env.TelegramBaseURL))
	}

	if em := in.Config.Notify.Email; em != nil {
		notifiers = append(notifiers, newEmailNotifier(in, *em))
	}

	return notifiers
}

// newEmailNotifier builds the email notifier from cfg and, only when a test set Env.SMTPAddr,
// points it at that address instead of cfg's host and port (config validation allows only the
// privileged ports 587 and 465 there, which a fake SMTP server cannot bind).
func newEmailNotifier(in BuildInput, cfg config.Email) *email.Notifier {
	n := email.New(cfg, in.Secrets.SMTPPassword, in.Env.SMTPTLSConfig)
	if in.Env.SMTPAddr != "" {
		n = n.WithAddr(in.Env.SMTPAddr)
	}

	return n
}
