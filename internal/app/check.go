package app

import (
	"context"
	"io"
	"log/slog"
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

// Deps is everything one check run needs, built by the caller so the run itself never reads the
// environment, the console or the real clock. State is read, never written: check remembers
// nothing between runs (FR-012). Close, when set, releases what the builder opened (the log file);
// the CLI calls it once the command is done, and Check itself never does.
type Deps struct {
	Config    *config.Config
	State     *state.State
	Provider  bank.Provider
	Firefly   *firefly.Client
	Notifiers []notify.Notifier
	Redactor  *redact.Redactor
	Log       *slog.Logger
	Now       func() time.Time
	Stdout    io.Writer
	Close     func()
}

// CheckOptions are the check command's flags. Stdout prints the digest instead of sending it
// (FR-013).
type CheckOptions struct {
	Stdout bool
}

// checkRun is one run's fixed context: the dependencies and the dates derived once from a single
// clock reading, so every account in the run is judged against the same "today" (FR-004).
type checkRun struct {
	deps      Deps
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
	today := civil.DateOf(deps.Now().In(deps.Config.Location))

	return &checkRun{
		deps:      deps,
		today:     today,
		window:    domain.NewWindow(today, deps.Config.WindowDays),
		tolerance: deps.Config.DateToleranceDays,
	}
}

// reconcileAll builds the report's accounts. A Firefly III account listing failure is a run-level
// problem that stops the run before any bank call, since no bank account could be mapped anyway.
func (r *checkRun) reconcileAll(ctx context.Context) report.RunReport {
	rep := report.RunReport{Window: r.window}

	ffAccounts, err := r.deps.Firefly.ListAccounts(ctx)
	if err != nil {
		r.deps.Log.InfoContext(ctx, "firefly accounts unavailable", "error", err)
		rep.Problems = append(rep.Problems, report.Problem{
			Scope:  "firefly",
			Reason: "Firefly III unreachable: " + r.deps.Redactor.Scrub(err.Error()),
		})

		return rep
	}

	for _, key := range r.sessionBanks() {
		session := r.deps.State.Sessions[key]

		mappings := mapping.Resolve(bankAccounts(key, &session), ffAccounts, r.deps.Config.Accounts)
		for i := range mappings {
			rep.Accounts = append(rep.Accounts, r.checkAccount(ctx, session.SessionID, mappings[i]))
		}
	}

	return rep
}

// sessionBanks returns the keys of the configured banks that have a session in the state, sorted
// so every run visits them in the same order. A session for a bank no longer configured is
// ignored.
func (r *checkRun) sessionBanks() []string {
	keys := make([]string, 0, len(r.deps.State.Sessions))

	for key := range r.deps.State.Sessions {
		if _, ok := r.deps.Config.Banks[key]; ok {
			keys = append(keys, key)
		}
	}

	slices.Sort(keys)

	return keys
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
