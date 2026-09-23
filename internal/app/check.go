package app

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/digest"
	"github.com/Toshik1978/firefly-jar/internal/domain"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
	"github.com/Toshik1978/firefly-jar/internal/mapping"
	"github.com/Toshik1978/firefly-jar/internal/notify"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/report"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Deps is everything one command run needs, built by the caller so the run itself never reads the
// environment, the console or the real clock. Check reads State and never writes it: check remembers
// nothing between runs (FR-012); only auth saves it. Authorizer is set for auth alone, so check and
// accounts can never start or revoke a consent. Close, when set, releases what the builder opened
// (the log file); the CLI calls it once the command is done, and Check itself never does.
type Deps struct {
	Config     *config.Config
	State      *state.State
	Provider   bank.Provider
	Authorizer bank.Authorizer
	Firefly    *firefly.Client
	Notifiers  []notify.Notifier
	Redactor   *redact.Redactor
	Log        *slog.Logger
	Now        func() time.Time
	Stdout     io.Writer
	Close      func()
}

// CheckOptions are the check command's flags. Stdout prints the digest instead of sending it
// (FR-013).
type CheckOptions struct {
	Stdout bool
}

// checkRun is one run's fixed context: the dependencies, the single clock reading in the configured
// zone and the dates derived from it, so every account and every consent in the run is judged
// against the same instant and the same "today" (FR-004).
type checkRun struct {
	deps      Deps
	now       time.Time
	today     civil.Date
	window    domain.Window
	tolerance int
}

// Check runs one reconciliation (FR-003) and returns its report and the process exit code. Firefly
// III accounts are listed before any bank call, so an unreachable Firefly III never spends bank
// quota; then every configured bank's accounts are mapped and reconciled in bank-key order; then
// the digest, if one is needed, is sent or printed; finally one summary record is logged.
func Check(ctx context.Context, deps Deps, opts CheckOptions) (report.RunReport, int) {
	run := newCheckRun(deps)

	rep := run.reconcileAll(ctx)
	run.deliver(ctx, &rep, opts)
	run.logSummary(ctx, &rep)

	return rep, rep.ExitCode()
}

// newCheckRun reads the clock exactly once and derives today and the window from it in the
// configured time zone, whatever location the clock's own value carries.
func newCheckRun(deps Deps) *checkRun {
	now := deps.Now().In(deps.Config.Location)
	today := civil.DateOf(now)

	return &checkRun{
		deps:      deps,
		now:       now,
		today:     today,
		window:    domain.NewWindow(today, deps.Config.WindowDays),
		tolerance: deps.Config.DateToleranceDays,
	}
}

// reconcileAll builds the report's accounts. A Firefly III account listing failure is a run-level
// problem that stops the run before any bank call, since no bank account could be mapped anyway.
func (r *checkRun) reconcileAll(ctx context.Context) report.RunReport {
	rep := report.RunReport{Window: r.window}

	r.warnStaleSessions(ctx)

	ffAccounts, err := r.deps.Firefly.ListAccounts(ctx)
	if err != nil {
		r.deps.Log.InfoContext(ctx, "firefly accounts unavailable", "error", err)
		rep.Problems = append(rep.Problems, report.Problem{
			Scope:  "firefly",
			Reason: "Firefly III unreachable: " + r.deps.Redactor.Scrub(err.Error()),
		})

		return rep
	}

	for _, key := range configuredBanks(r.deps.Config.Banks) {
		r.reconcileBank(ctx, &rep, key, ffAccounts)
	}

	return rep
}

// reconcileBank adds one configured bank's accounts to rep. An expired consent unchecks every
// account without a bank call; once the provider reports the consent expired or revoked for one
// account, the rest of the bank's accounts are unchecked the same way rather than asked for again.
func (r *checkRun) reconcileBank(ctx context.Context, rep *report.RunReport, key string, ffAccounts []firefly.Account) {
	session, ok := r.usableSession(ctx, rep, key)
	if !ok {
		return
	}

	var (
		lostCode report.UncheckedCode
		lost     bool
	)

	switch consentState(session, r.now, r.deps.Config.ConsentWarnDays) {
	case consentExpired:
		lostCode, lost = report.ConsentExpired, true
	case consentExpiring:
		rep.ConsentWarnings = append(rep.ConsentWarnings, r.consentWarning(key, session))
	case consentOK:
	}

	mappings := mapping.Resolve(bankAccounts(key, session), ffAccounts, r.deps.Config.Accounts)
	for i := range mappings {
		if lost {
			rep.Accounts = append(rep.Accounts, r.consentUnchecked(ctx, mappings[i], lostCode))

			continue
		}

		res := r.checkAccount(ctx, session.SessionID, mappings[i])
		lostCode, lost = consentLost(&res)
		rep.Accounts = append(rep.Accounts, res)
	}
}

// deliver renders the digest when the report needs one and either prints it (--stdout) or fans it
// out to every notifier, recording how delivery went. A digest that could not be printed is a
// run-level problem, so the run never ends 0 or 1 without the owner having seen it.
func (r *checkRun) deliver(ctx context.Context, rep *report.RunReport, opts CheckOptions) {
	if !rep.DigestNeeded() {
		return
	}

	d := digest.Render(*rep, r.deps.Config.Banks)

	if !opts.Stdout {
		rep.Delivery = notify.FanOut(ctx, r.deps.Notifiers, d, r.deps.Redactor)

		return
	}

	if _, err := io.WriteString(r.deps.Stdout, d.Text()); err != nil {
		r.deps.Log.ErrorContext(ctx, "print digest failed", "error", err)
		rep.Problems = append(rep.Problems, report.Problem{
			Scope:  "stdout",
			Reason: "digest not printed: " + r.deps.Redactor.Scrub(err.Error()),
		})
	}
}

// logSummary writes the run's one INFO summary record (FR-038). Only counts and dates appear in
// it, never a description.
func (r *checkRun) logSummary(ctx context.Context, rep *report.RunReport) {
	sum := rep.Summary()

	r.deps.Log.InfoContext(ctx, "run summary",
		slog.String("window", rep.Window.From.String()+".."+rep.Window.To.String()),
		slog.Int("accounts_checked", sum.AccountsChecked),
		slog.Int("accounts_unchecked", sum.AccountsUnchecked),
		slog.Int("matched", sum.Matched),
		slog.Int("missing", sum.Missing),
		slog.Int("deduplicated", sum.Deduplicated),
		slog.Int("void", sum.Void),
	)
}

// bankAccounts turns one session's stored accounts into bank accounts carrying their bank key.
func bankAccounts(key string, session *state.Session) []bank.Account {
	accounts := make([]bank.Account, 0, len(session.Accounts))

	for _, a := range session.Accounts {
		accounts = append(accounts, bank.Account{
			BankKey:  key,
			UID:      a.UID,
			Hash:     a.Hash,
			IBAN:     a.IBAN,
			Currency: a.Currency,
			Name:     a.Name,
		})
	}

	return accounts
}

// configuredBanks returns the configured bank keys sorted, so every run visits them in the same
// order.
func configuredBanks(banks map[string]config.Bank) []string {
	return slices.Sorted(maps.Keys(banks))
}
