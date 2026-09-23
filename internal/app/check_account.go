package app

import (
	"context"
	"errors"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/firefly"
	"github.com/Toshik1978/firefly-jar/internal/mapping"
	"github.com/Toshik1978/firefly-jar/internal/reconcile"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/report"
)

// checkAccount reconciles one mapped bank account, or records why it could not be. An account
// that cannot be mapped costs no bank call: its transactions could not be compared with anything.
func (r *checkRun) checkAccount(ctx context.Context, sessionID string, m mapping.Mapping) report.AccountResult {
	switch {
	case m.Status == mapping.Excluded:
		return report.AccountResult{Mapping: m}
	case m.Status == mapping.Unmapped && m.TargetID != "":
		// An accounts: override named a Firefly III account id nothing has (data-model.md
		// "Mapping" resolution order step 1). m.Detail already carries the full sentence for the
		// accounts command; the digest wants only the bare id, so it is not repeated twice.
		return r.unchecked(ctx, m, report.OverrideTargetMissing, "#"+m.TargetID)
	case m.Status == mapping.Unmapped:
		return r.unchecked(ctx, m, report.Unmapped, "")
	case m.Status == mapping.Ambiguous:
		return r.unchecked(ctx, m, report.Ambiguous, "")
	}

	txs, err := r.deps.Provider.Transactions(ctx, sessionID, m.Bank, r.window.From)
	if err != nil {
		code, detail := r.bankFailure(err)

		return r.uncheckedErr(ctx, m, code, detail, err)
	}

	// The Firefly III range is widened by the tolerance on both sides, so a bank transaction on the
	// window's first or last day can still meet an entry the owner dated a few days apart (FR-006).
	entries, err := r.deps.Firefly.ListAccountTransactions(ctx, m.Firefly.ID, m.Bank.Currency,
		r.window.From.AddDays(-r.tolerance), r.today.AddDays(r.tolerance))
	if err != nil {
		code, detail := r.fireflyFailure(err)

		return r.uncheckedErr(ctx, m, code, detail, err)
	}

	res := reconcile.Reconcile(txs, entries, r.tolerance, r.window)

	r.deps.Log.DebugContext(ctx, "account reconciled",
		"bank", m.Bank.BankKey,
		"account", maskedAccount(&m.Bank),
		"firefly_account_id", m.Firefly.ID,
		"matched", len(res.Matched),
		"missing", len(res.Missing),
	)

	return report.AccountResult{Mapping: m, Result: res}
}

// unchecked records that m could not be reconciled for code.
func (r *checkRun) unchecked(
	ctx context.Context, m mapping.Mapping, code report.UncheckedCode, detail string,
) report.AccountResult {
	r.deps.Log.InfoContext(ctx, "account unchecked",
		"bank", m.Bank.BankKey,
		"account", maskedAccount(&m.Bank),
		"reason", code.Text(),
	)

	return report.AccountResult{Mapping: m, Unchecked: &report.Unchecked{Code: code, Detail: detail}}
}

// uncheckedErr records that a bank or Firefly III call for m failed with err. The error goes to the
// file log only; the stderr summary of a failed run is not this function's job.
func (r *checkRun) uncheckedErr(
	ctx context.Context, m mapping.Mapping, code report.UncheckedCode, detail string, err error,
) report.AccountResult {
	r.deps.Log.InfoContext(ctx, "account unchecked",
		"bank", m.Bank.BankKey,
		"account", maskedAccount(&m.Bank),
		"reason", code.Text(),
		"error", err,
	)

	return report.AccountResult{Mapping: m, Unchecked: &report.Unchecked{Code: code, Detail: detail}}
}

// bankFailure classifies a bank provider error. The sentinel kinds are fully described by their
// code; only an unclassified failure carries its redacted detail into the digest.
func (r *checkRun) bankFailure(err error) (report.UncheckedCode, string) {
	switch {
	case errors.Is(err, bank.ErrRateLimited):
		return report.RateLimited, ""
	case errors.Is(err, bank.ErrDataIncomplete):
		return report.BankDataIncomplete, ""
	case errors.Is(err, bank.ErrConsentExpired):
		return report.ConsentExpired, ""
	case errors.Is(err, bank.ErrConsentRevoked):
		return report.ConsentRevoked, ""
	}

	if bankErr, ok := errors.AsType[*bank.Error](err); ok {
		return report.BankError, r.deps.Redactor.Scrub(bankErr.Detail)
	}

	return report.BankError, r.deps.Redactor.Scrub(err.Error())
}

// fireflyFailure classifies a Firefly III transaction listing error for one account.
func (r *checkRun) fireflyFailure(err error) (report.UncheckedCode, string) {
	if errors.Is(err, firefly.ErrDataIncomplete) {
		return report.FireflyDataIncomplete, ""
	}

	return report.FireflyError, r.deps.Redactor.Scrub(err.Error())
}

// maskedAccount identifies a bank account in a log record without exposing it: the masked IBAN,
// or the masked hash for an account the bank reports no IBAN for.
func maskedAccount(acc *bank.Account) string {
	if acc.IBAN != "" {
		return redact.MaskIBAN(acc.IBAN)
	}

	return redact.MaskHash(acc.Hash)
}
