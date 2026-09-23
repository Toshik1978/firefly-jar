package notify_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/digest"
	"github.com/Toshik1978/firefly-jar/internal/notify"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/report"
)

// ctxKey is a unique type for the marker value TestFanOutPassesContextThroughToSend stashes on its
// context, so FanOut passing the caller's ctx unchanged through to Send can be observed directly.
type ctxKey struct{}

// recordingNotifier is a fake notify.Notifier that returns a fixed set of results and, for whichever
// slices are non-nil, records every Send call's name, digest and context, so a suite method can
// assert call order and that every notifier sees the same digest.Digest.
type recordingNotifier struct {
	name    string
	results []notify.Result
	calls   *[]string
	digests *[]digest.Digest
	ctxs    *[]context.Context
}

// Name returns n's fixed channel name.
func (n *recordingNotifier) Name() string {
	return n.name
}

// Send records this call, when the suite asked it to, and returns n's fixed results.
func (n *recordingNotifier) Send(ctx context.Context, d digest.Digest) []notify.Result {
	if n.calls != nil {
		*n.calls = append(*n.calls, n.name)
	}

	if n.digests != nil {
		*n.digests = append(*n.digests, d)
	}

	if n.ctxs != nil {
		*n.ctxs = append(*n.ctxs, ctx)
	}

	return n.results
}

// FanOutSuite covers notify.FanOut and notify.MaskRecipient (FR-023, FR-028): every notifier is
// called regardless of an earlier one's outcome, results merge into one report.Delivery, and every
// recipient and failure reason leaving the package is masked and scrubbed.
type FanOutSuite struct {
	suite.Suite
}

// TestFanOutCallsEveryNotifierEvenWhenAnEarlierOneFailsEntirely asserts that a notifier whose every
// result is a failure never stops FanOut from calling the next notifier, in registration order, and
// that both notifiers receive the exact same digest.
func (s *FanOutSuite) TestFanOutCallsEveryNotifierEvenWhenAnEarlierOneFailsEntirely() {
	d := digest.Digest{
		Subject: "firefly-jar: 1 missing, 0 unchecked accounts (window 2026-09-01 – 2026-09-23)",
		Lines:   []string{"firefly-jar: 1 missing, 0 unchecked accounts (window 2026-09-01 – 2026-09-23)"},
	}

	var calls []string

	var gotDigests []digest.Digest

	telegram := &recordingNotifier{
		name: "telegram",
		results: []notify.Result{
			{Recipient: "123456789", Err: errors.New("network timeout")},
			{Recipient: "-1001234567890", Err: errors.New("network timeout")},
		},
		calls:   &calls,
		digests: &gotDigests,
	}
	email := &recordingNotifier{
		name:    "email",
		results: []notify.Result{{Recipient: "a@example.com", Err: nil}},
		calls:   &calls,
		digests: &gotDigests,
	}

	got := notify.FanOut(context.Background(), []notify.Notifier{telegram, email}, d, redact.New())

	s.Equal([]string{"telegram", "email"}, calls, "email must still be called after telegram fails entirely")
	s.Require().Len(gotDigests, 2)
	s.Equal(d, gotDigests[0])
	s.Equal(d, gotDigests[1])
	s.Equal(3, got.Attempted)
	s.Equal(1, got.Succeeded)
	s.Equal([]report.DeliveryFailure{
		{Channel: "telegram", Recipient: "…6789", Reason: "network timeout"},
		{Channel: "telegram", Recipient: "…7890", Reason: "network timeout"},
	}, got.Failures, "both of telegram's failures must survive the merge, in result order")
}

// TestFanOutMergesResultsFromEveryNotifierIntoOneDelivery pins the exact merge FR-028 promises:
// counts add across notifiers, and each failure carries its own notifier's channel name.
func (s *FanOutSuite) TestFanOutMergesResultsFromEveryNotifierIntoOneDelivery() {
	d := digest.Digest{Subject: "firefly-jar: 2 missing", Lines: []string{"firefly-jar: 2 missing"}}

	telegram := &recordingNotifier{
		name: "telegram",
		results: []notify.Result{
			{Recipient: "123456789", Err: nil},
			{Recipient: "-1001234567890", Err: errors.New("network timeout")},
		},
	}
	email := &recordingNotifier{
		name: "email",
		results: []notify.Result{
			{Recipient: "a@example.com", Err: nil},
			{Recipient: "b@example.com", Err: nil},
		},
	}

	got := notify.FanOut(context.Background(), []notify.Notifier{telegram, email}, d, redact.New())

	s.Equal(report.Delivery{
		Attempted: 4,
		Succeeded: 3,
		Failures: []report.DeliveryFailure{
			{Channel: "telegram", Recipient: "…7890", Reason: "network timeout"},
		},
	}, got)
}

// TestFanOutScrubsSecretsAndIBANsFromFailureReasons asserts that a failure Reason never carries a
// configured secret or a bare IBAN out of the package, whichever notifier produced it.
func (s *FanOutSuite) TestFanOutScrubsSecretsAndIBANsFromFailureReasons() {
	d := digest.Digest{Subject: "firefly-jar: 1 missing", Lines: []string{"firefly-jar: 1 missing"}}
	secret := "s3cr3t-token-aaa"
	iban := "LT121000011101001000"

	telegram := &recordingNotifier{
		name: "telegram",
		results: []notify.Result{
			{Recipient: "123456789", Err: fmt.Errorf("auth failed for %s using token %s", iban, secret)},
		},
	}

	got := notify.FanOut(context.Background(), []notify.Notifier{telegram}, d, redact.New(secret))

	s.Require().Len(got.Failures, 1)
	s.Equal("auth failed for LT12…1000 using token [REDACTED]", got.Failures[0].Reason)
	s.NotContains(got.Failures[0].Reason, secret)
	s.NotContains(got.Failures[0].Reason, iban)
}

// TestFanOutWithNoNotifiersReturnsZeroDelivery asserts the degenerate case never fabricates an
// attempt.
func (s *FanOutSuite) TestFanOutWithNoNotifiersReturnsZeroDelivery() {
	d := digest.Digest{Subject: "firefly-jar: 0 missing", Lines: []string{"firefly-jar: 0 missing"}}

	got := notify.FanOut(context.Background(), nil, d, redact.New())

	s.Equal(report.Delivery{}, got)
}

// TestFanOutPassesContextThroughToSend asserts FanOut forwards the caller's context unchanged, to
// every notifier, rather than deriving a new one (e.g. with its own timeout) that would still carry
// the same value but no longer be the identical context.Context. A value-only check would let a
// derived context slip through, so this compares ctx itself.
func (s *FanOutSuite) TestFanOutPassesContextThroughToSend() {
	d := digest.Digest{Subject: "firefly-jar: 1 missing", Lines: []string{"firefly-jar: 1 missing"}}
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")

	var ctxs []context.Context

	telegram := &recordingNotifier{
		name:    "telegram",
		results: []notify.Result{{Recipient: "123456789", Err: nil}},
		ctxs:    &ctxs,
	}
	email := &recordingNotifier{
		name:    "email",
		results: []notify.Result{{Recipient: "a@example.com", Err: nil}},
		ctxs:    &ctxs,
	}

	notify.FanOut(ctx, []notify.Notifier{telegram, email}, d, redact.New())

	s.Require().Len(ctxs, 2)
	s.Equal(ctx, ctxs[0], "telegram must receive the identical context, not a derived one")
	s.Equal(ctx, ctxs[1], "email must receive the identical context, not a derived one")
}

// TestMaskRecipient pins the exact masking rules the fj-xwu.3.20 ruling fixed: an email keeps its
// first character and domain, anything else keeps its last four characters, and a recipient of four
// characters or fewer never shows enough to identify it.
func (s *FanOutSuite) TestMaskRecipient() {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "email keeps first character and domain", in: "me@example.com", want: "m…@example.com"},
		{name: "telegram chat id keeps last four", in: "123456789", want: "…6789"},
		{name: "negative telegram group id keeps last four", in: "-1001234567890", want: "…7890"},
		{name: "four characters becomes asterisks", in: "1234", want: "****"},
		{name: "empty string becomes asterisks", in: "", want: "****"},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			s.Equal(tc.want, notify.MaskRecipient(tc.in))
		})
	}
}
