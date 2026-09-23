// Package telegram delivers a digest through the Telegram Bot API's sendMessage method as plain
// text (research R9). A digest longer than one message is split at line boundaries, parts to one
// chat are paced, and the notifier owns its retry policy, because sendMessage is a POST that the
// shared httpclient transport never retries and Telegram signals flood control in the body, not in a
// Retry-After header.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Toshik1978/firefly-jar/internal/digest"
	"github.com/Toshik1978/firefly-jar/internal/notify"
)

const (
	// defaultBaseURL is the Bot API that New uses when it is given an empty base URL.
	defaultBaseURL = "https://api.telegram.org"
	// channelName is what Name reports, and so the channel of every delivery failure.
	channelName = "telegram"
	// partGap keeps parts to one chat under Telegram's limit of about one message per second
	// per chat (research R9).
	partGap = 1100 * time.Millisecond
	// redacted replaces the bot token wherever it would otherwise reach an error.
	redacted = "<redacted>"
)

// Notifier sends a digest to a fixed list of Telegram chats through one bot. It is safe to reuse
// across Send calls but holds no state between them.
type Notifier struct {
	hc       *http.Client
	endpoint string
	secrets  []string
	chatIDs  []int64
}

// New returns a Notifier that posts through hc to baseURL's sendMessage method for token, once
// per chat id, in the order given. An empty baseURL means the public Bot API; tests point it at a
// fake. A nil hc means http.DefaultClient.
func New(token string, chatIDs []int64, hc *http.Client, baseURL string) *Notifier {
	if hc == nil {
		hc = http.DefaultClient
	}

	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	return &Notifier{
		hc:       hc,
		endpoint: strings.TrimRight(baseURL, "/") + "/bot" + token + "/sendMessage",
		secrets:  tokenSecrets(token),
		chatIDs:  slices.Clone(chatIDs),
	}
}

// Name reports the channel name, "telegram".
func (n *Notifier) Name() string {
	return channelName
}

// Send delivers d to every configured chat, one chat after another, and returns one Result per
// chat in the configured order, with the chat id in decimal as the Recipient. A failure at one
// chat never stops the next. An error names the masked chat id and the HTTP status or network
// failure, never the bot token, the request URL or the Bot API's description, which could echo
// the message text.
func (n *Notifier) Send(ctx context.Context, d digest.Digest) []notify.Result {
	parts := split(d.Lines)
	results := make([]notify.Result, 0, len(n.chatIDs))

	for _, chatID := range n.chatIDs {
		recipient := strconv.FormatInt(chatID, 10)
		result := notify.Result{Recipient: recipient}

		if err := n.sendParts(ctx, chatID, parts); err != nil {
			result.Err = n.scrub(fmt.Errorf("telegram: chat %s: %w", notify.MaskRecipient(recipient), err))
		}

		results = append(results, result)
	}

	return results
}

// sendParts sends parts to chatID in order, at least partGap apart, and stops at the first part
// that fails: a later part without the earlier ones would read as a complete digest.
func (n *Notifier) sendParts(ctx context.Context, chatID int64, parts []string) error {
	for i, text := range parts {
		if i > 0 {
			if err := sleep(ctx, partGap); err != nil {
				return fmt.Errorf("part %d/%d: wait before sending: %w", i+1, len(parts), err)
			}
		}

		if err := n.sendPart(ctx, chatID, text); err != nil {
			if len(parts) == 1 {
				return err
			}

			return fmt.Errorf("part %d/%d: %w", i+1, len(parts), err)
		}
	}

	return nil
}

// sendPart posts one message, retrying as the attempt's outcome allows, at most maxRetries times.
// Retries are not paced by partGap: the wait they get is the one the failure asked for.
func (n *Notifier) sendPart(ctx context.Context, chatID int64, text string) error {
	body, err := json.Marshal(sendMessageRequest{
		Text:               text,
		ChatID:             chatID,
		LinkPreviewOptions: linkPreviewOptions{IsDisabled: true},
	})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("not sent: %w", err)
		}

		out := n.attempt(ctx, body, attempt)
		if out.err == nil {
			return nil
		}

		if !out.retry || attempt >= maxRetries {
			return out.err
		}

		if err := sleep(ctx, out.wait); err != nil {
			return fmt.Errorf("%w; wait before retry: %w", out.err, err)
		}
	}
}

// scrub is the last line of defense for the token: every error built here already avoids the
// request URL, but should a transport error ever quote it, the token is replaced. Only then is
// the error chain given up, since secrecy outranks errors.Is.
func (n *Notifier) scrub(err error) error {
	msg := err.Error()

	clean := msg
	for _, secret := range n.secrets {
		clean = strings.ReplaceAll(clean, secret, redacted)
	}

	if clean == msg {
		return err
	}

	return errors.New(clean)
}

// tokenSecrets returns the strings scrub must never let through: the whole token and, since a
// bot token is "<bot id>:<secret>", its secret half on its own. Empty strings are left out, as
// replacing "" would insert the marker between every character.
func tokenSecrets(token string) []string {
	var secrets []string

	if token != "" {
		secrets = append(secrets, token)
	}

	if _, secret, ok := strings.Cut(token, ":"); ok && secret != "" {
		secrets = append(secrets, secret)
	}

	return secrets
}

// sleep waits d, or returns early with the context's error once ctx is done, so a cancelled run
// never sits out a 60-second retry_after.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("wait %s: %w", d, ctx.Err())
	case <-timer.C:
		return nil
	}
}
