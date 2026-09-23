// Package notify fans a rendered digest.Digest out to every configured notifier and merges what
// came back into one report.Delivery (FR-023, FR-028). It masks every recipient and scrubs every
// failure reason itself, so no notifier implementation has to remember to.
package notify

import (
	"context"
	"strings"

	"github.com/Toshik1978/firefly-jar/internal/digest"
	"github.com/Toshik1978/firefly-jar/internal/redact"
	"github.com/Toshik1978/firefly-jar/internal/report"
)

// maskedRecipient is what MaskRecipient returns for a recipient too short to show any of it
// without exposing most of it.
const maskedRecipient = "****"

// visibleRecipientSuffix is how many trailing characters MaskRecipient leaves visible on a
// non-email recipient.
const visibleRecipientSuffix = 4

// Notifier delivers a rendered digest to whatever recipients it is configured with. Send returns
// one Result per recipient attempted, with the raw (unmasked) recipient and the raw error, since
// FanOut is the single point that masks and redacts before either leaves the package.
type Notifier interface {
	Name() string
	Send(ctx context.Context, d digest.Digest) []Result
}

// Result is the outcome of sending to one recipient: Recipient is raw (chat id, email address,
// …) and Err is nil on success.
type Result struct {
	Recipient string
	Err       error
}

// FanOut sends d through every notifier in notifiers, in order, using the caller's ctx unchanged
// for every call. A notifier whose results are entirely failures never stops the next notifier
// from being called. Every recipient and failure reason is masked and scrubbed with r before it
// is merged into the returned report.Delivery; a nil r behaves like redact.New().
func FanOut(ctx context.Context, notifiers []Notifier, d digest.Digest, r *redact.Redactor) report.Delivery {
	if r == nil {
		r = redact.New()
	}

	var delivery report.Delivery

	for _, n := range notifiers {
		for _, res := range n.Send(ctx, d) {
			delivery.Attempted++

			if res.Err == nil {
				delivery.Succeeded++
				continue
			}

			delivery.Failures = append(delivery.Failures, report.DeliveryFailure{
				Channel:   n.Name(),
				Recipient: MaskRecipient(res.Recipient),
				Reason:    r.Scrub(res.Err.Error()),
			})
		}
	}

	return delivery
}

// MaskRecipient masks a raw recipient before it leaves the notify package: an email address
// keeps its first character and its domain ("m…@example.com"), and anything else keeps its last
// visibleRecipientSuffix characters ("…6789"). A recipient of visibleRecipientSuffix characters or
// fewer, email or not, comes back as maskedRecipient, since showing any of it would show most of
// it.
func MaskRecipient(recipient string) string {
	if at := strings.IndexByte(recipient, '@'); at >= 0 {
		local := []rune(recipient[:at])
		if len(local) == 0 {
			return maskedRecipient
		}

		return string(local[:1]) + "…" + recipient[at:]
	}

	runes := []rune(recipient)
	if len(runes) <= visibleRecipientSuffix {
		return maskedRecipient
	}

	return "…" + string(runes[len(runes)-visibleRecipientSuffix:])
}
