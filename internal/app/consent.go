package app

import (
	"context"
	"slices"
	"time"

	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/mapping"
	"github.com/Toshik1978/firefly-jar/internal/report"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// consent is a saved session's consent state at one instant (data-model.md "Consent state"). A
// bank with no session, or with a session covering no accounts, never reaches this far: its
// accounts are unknown, so it is a run-level problem instead.
type consent int

const (
	// consentOK is a consent further than warn_days from expiry.
	consentOK consent = iota
	// consentExpiring is a consent within warn_days of expiry: warned about, still checked.
	consentExpiring
	// consentExpired is a consent whose ValidUntil has been reached: nothing of the bank is checked.
	consentExpired
)

// consentState places session in the consent table relative to now. The warning window is counted
// in calendar days in now's location rather than in 24-hour blocks, so a daylight-saving change
// inside it never moves the warning by an hour.
func consentState(session *state.Session, now time.Time, warnDays int) consent {
	switch {
	case !now.Before(session.ValidUntil):
		return consentExpired
	case !now.Before(session.ValidUntil.In(now.Location()).AddDate(0, 0, -warnDays)):
		return consentExpiring
	default:
		return consentOK
	}
}

// sessionProblem names why key's saved session cannot be used — no session at all, or one that saw
// no accounts — and how to fix it. Both check (consent.go) and accounts (accounts.go) hit exactly
// these two cases and report the same fix, so both share this one wording (fix round 1 on
// fj-xwu.5.4: accounts must not silently drop a bank in either case). ok is false when reason is set,
// true when the session is safe to use as-is.
func sessionProblem(session state.Session, ok bool, key string) (string, bool) {
	switch {
	case !ok:
		return "not authorized — run: firefly-jar auth " + key, false
	case len(session.Accounts) == 0:
		return "no accounts in the saved session — run: firefly-jar auth " + key, false
	default:
		return "", true
	}
}

// usableSession returns the bank's saved session, or records a run-level problem and reports false
// when there is nothing to check: no session at all, or a session that saw no accounts. Both are
// fixed the same way, by authorizing the bank again, so both carry the same hint.
func (r *checkRun) usableSession(ctx context.Context, rep *report.RunReport, key string) (*state.Session, bool) {
	session, ok := r.deps.State.Sessions[key]

	reason, usable := sessionProblem(session, ok, key)
	if usable {
		return &session, true
	}

	r.deps.Log.InfoContext(ctx, "bank not checked", "bank", key, "reason", reason)
	rep.Problems = append(rep.Problems, report.Problem{Scope: key, Reason: reason})

	return nil, false
}

// consentWarning describes an expiring consent. ValidUntil is carried in the configured zone, the
// zone the digest shows its calendar date in, and DaysLeft counts whole calendar days from today.
func (r *checkRun) consentWarning(key string, session *state.Session) report.ConsentWarning {
	validUntil := session.ValidUntil.In(r.deps.Config.Location)

	return report.ConsentWarning{
		BankKey:    key,
		ValidUntil: validUntil,
		DaysLeft:   civil.DateOf(validUntil).DaysSince(r.today),
	}
}

// consentUnchecked records that m was not checked because its bank's consent is gone. An excluded
// account stays excluded: the owner asked for it never to be checked or reported, consent or not.
func (r *checkRun) consentUnchecked(
	ctx context.Context, m mapping.Mapping, code report.UncheckedCode,
) report.AccountResult {
	if m.Status == mapping.Excluded {
		return report.AccountResult{Mapping: m}
	}

	return r.unchecked(ctx, m, code, "")
}

// warnStaleSessions logs one warning per saved session whose bank is no longer configured. Such a
// session is never used: nothing else in the run could tell the owner it is still on disk.
func (r *checkRun) warnStaleSessions(ctx context.Context) {
	var stale []string

	for key := range r.deps.State.Sessions {
		if _, ok := r.deps.Config.Banks[key]; !ok {
			stale = append(stale, key)
		}
	}

	slices.Sort(stale)

	for _, key := range stale {
		r.deps.Log.WarnContext(ctx, "saved session for a bank not in config ignored", "bank", key)
	}
}

// consentLost reports whether res failed because the provider says the bank's consent is gone, in
// which case no other account of the same bank can succeed either.
func consentLost(res *report.AccountResult) (report.UncheckedCode, bool) {
	if res.Unchecked == nil {
		return 0, false
	}

	code := res.Unchecked.Code

	return code, code == report.ConsentExpired || code == report.ConsentRevoked
}
