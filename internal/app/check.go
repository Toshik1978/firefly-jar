package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/Toshik1978/firefly-jar/internal/accountmap"
	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/digest"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
	"github.com/Toshik1978/firefly-jar/internal/notify"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/report"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Deps is everything one command run needs, built by the caller so the run itself never reads the
// environment, the console or the real clock. Check reads State and never writes it: check remembers
// nothing between runs (FR-012); only auth saves it. Authorizer is set for auth alone, so check and
// accounts can never start or revoke a consent. Stderr is used only for the FR-028 fallback: the full
// digest text, when every delivery failed. Close, when set, releases what the builder opened
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
	Stderr     io.Writer
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
	window    civil.Range
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

	code := rep.ExitCode()
	if code == 2 {
		run.logFailure(ctx, &rep)
	}

	return rep, code
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
		window:    civil.NewRange(today, deps.Config.WindowDays),
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
		rep.Problems = append(rep.Problems, report.Problem{Scope: "firefly", Reason: r.fireflyProblem(err)})

		return rep
	}

	for _, key := range configuredBanks(r.deps.Config.Banks) {
		r.reconcileBank(ctx, &rep, key, ffAccounts)
	}

	return rep
}

// fireflyProblem words a failed Firefly III accounts listing for the digest. A rejected token is
// named as such, with where the token comes from, since the server did answer and "unreachable"
// would send the owner looking at the network; anything else is "unreachable" with the scrubbed
// error.
func (r *checkRun) fireflyProblem(err error) string {
	if errors.Is(err, firefly.ErrUnauthorized) {
		return "Firefly III unauthorized: the API token was rejected — check firefly.token_file or " +
			"FIREFLY_JAR_FIREFLY_TOKEN"
	}

	return "Firefly III unreachable: " + r.deps.Redactor.Scrub(err.Error())
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

	mappings := accountmap.Resolve(bankAccounts(key, session), ffAccounts, r.deps.Config.Accounts)
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
// run-level problem, so the run never ends 0 or 1 without the owner having seen it. When every
// notifier failed for every recipient, the digest still reaches the owner as the FR-028 stderr
// fallback, alongside a WARN already logged for each individual failure.
func (r *checkRun) deliver(ctx context.Context, rep *report.RunReport, opts CheckOptions) {
	if !rep.DigestNeeded() {
		return
	}

	d := digest.Render(*rep, r.deps.Config.Banks)

	if opts.Stdout {
		r.printDigest(ctx, rep, d)

		return
	}

	rep.Delivery = notify.FanOut(ctx, r.deps.Notifiers, d, r.deps.Redactor)
	r.warnDeliveryFailures(ctx, rep.Delivery.Failures)
	r.fallbackToStderr(ctx, rep.Delivery, d)
}

// printDigest writes d to stdout for a --stdout run. A write failure is a run-level problem, so
// the run never ends 0 or 1 without the owner having seen the digest some other way.
func (r *checkRun) printDigest(ctx context.Context, rep *report.RunReport, d digest.Digest) {
	if _, err := io.WriteString(r.deps.Stdout, d.Text()); err != nil {
		r.deps.Log.ErrorContext(ctx, "print digest failed", "error", err)
		rep.Problems = append(rep.Problems, report.Problem{
			Scope:  "stdout",
			Reason: "digest not printed: " + r.deps.Redactor.Scrub(err.Error()),
		})
	}
}

// warnDeliveryFailures logs one WARN per failed recipient (FR-028), whatever else succeeded, so
// cron's mail-on-output always names every recipient the digest did not reach.
func (r *checkRun) warnDeliveryFailures(ctx context.Context, failures []report.DeliveryFailure) {
	for _, f := range failures {
		r.deps.Log.WarnContext(ctx, "delivery failed",
			"channel", f.Channel, "recipient", f.Recipient, "reason", f.Reason)
	}
}

// fallbackToStderr writes d's full text to stderr when delivery failed for every recipient across
// every channel (FR-028): the owner still sees the digest even though no channel delivered it. A
// partial failure, or no attempt at all, leaves stderr to the per-recipient WARNs alone.
func (r *checkRun) fallbackToStderr(ctx context.Context, delivery report.Delivery, d digest.Digest) {
	if delivery.Attempted == 0 || delivery.Succeeded > 0 {
		return
	}

	if _, err := io.WriteString(r.deps.Stderr, d.Text()); err != nil {
		r.deps.Log.ErrorContext(ctx, "stderr fallback failed", "error", err)
	}
}

// logSummary writes the run's one INFO summary record (FR-038, constitution §VI "plus any
// errors"). Only counts and dates appear in it, never a description; problems and delivery_failed
// carry the same run-level error counts the stderr ERROR line uses on an exit-2 run.
func (r *checkRun) logSummary(ctx context.Context, rep *report.RunReport) {
	sum := rep.Summary()

	r.deps.Log.InfoContext(ctx, "run summary",
		slog.String("window", rep.Window.From.String()+".."+rep.Window.To.String()),
		slog.Int("accounts_checked", sum.AccountsChecked),
		slog.Int("accounts_unchecked", sum.AccountsUnchecked),
		slog.Int("accounts_excluded", sum.AccountsExcluded),
		slog.Int("matched", sum.Matched),
		slog.Int("missing", sum.Missing),
		slog.Int("deduplicated", sum.Deduplicated),
		slog.Int("void", sum.Void),
		slog.Int("problems", len(rep.Problems)),
		slog.Int("delivery_failed", rep.Delivery.Attempted-rep.Delivery.Succeeded),
	)
}

// logFailure writes the run's one ERROR summary line to stderr (FR-032, FR-033, FR-039,
// contracts/cli.md) on every exit-2 run, so cron's mail-on-output carries a single, greppable
// reason the run failed instead of the full log. Exit 0 and 1 stay silent on stderr; this is
// called only when ExitCode() is 2.
func (r *checkRun) logFailure(ctx context.Context, rep *report.RunReport) {
	sum := rep.Summary()

	r.deps.Log.ErrorContext(ctx, "check failed",
		slog.Int("unchecked", sum.AccountsUnchecked),
		slog.Int("problems", len(rep.Problems)),
		slog.Int("delivery_failed", rep.Delivery.Attempted-rep.Delivery.Succeeded),
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
