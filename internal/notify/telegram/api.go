package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	// maxRetries caps the retries of one part on top of its first attempt; a 429 counts toward
	// it like a 5xx or a network error.
	maxRetries = 3
	// baseDelay is the wait before the first retry of a 5xx, a network error or a 429 without
	// parameters.retry_after, doubled for each retry after that: 1s, 2s, 4s.
	baseDelay = time.Second
	// maxRetryAfterSeconds caps parameters.retry_after. A longer wait fails the recipient at
	// once instead of stalling the run (research R12's 60s cap).
	maxRetryAfterSeconds = 60
	// maxReplyBytes bounds how much of a reply is read; a Bot API reply is a few hundred bytes.
	maxReplyBytes = 64 << 10
)

// sendMessageRequest is the sendMessage body. It carries no parse_mode: plain text means an
// arbitrary bank description can never fail as malformed markup (research R9).
type sendMessageRequest struct {
	Text               string             `json:"text"`
	ChatID             int64              `json:"chat_id"`
	LinkPreviewOptions linkPreviewOptions `json:"link_preview_options"`
}

// linkPreviewOptions turns off link previews, which would otherwise expand a URL that happens to
// sit in a transaction description.
type linkPreviewOptions struct {
	IsDisabled bool `json:"is_disabled"`
}

// apiReply holds the fields of a Bot API reply the notifier acts on. The description is left out
// on purpose: it is free text that could echo the message.
type apiReply struct {
	Parameters *replyParameters `json:"parameters"`
	ErrorCode  int              `json:"error_code"`
	OK         bool             `json:"ok"`
}

// replyParameters carries flood control's wait, which Telegram sends in the body rather than in a
// Retry-After header.
type replyParameters struct {
	RetryAfter int `json:"retry_after"`
}

// outcome is what one attempt concluded: success (a nil err), a failure to give up on, or a
// failure worth retrying after wait.
type outcome struct {
	err   error
	wait  time.Duration
	retry bool
}

// statusError reports a reply that was not a delivered message, by HTTP status and, when it
// differs, the Bot API's error_code (an ok:false on HTTP 200).
type statusError struct {
	status    int
	errorCode int
}

// Error implements the error interface for statusError.
func (e *statusError) Error() string {
	if e.errorCode != 0 && e.errorCode != e.status {
		return fmt.Sprintf("HTTP %d (error_code %d)", e.status, e.errorCode)
	}

	return fmt.Sprintf("HTTP %d", e.status)
}

// attempt posts body once and classifies the result; attemptNo (0-indexed) sets the backoff.
func (n *Notifier) attempt(ctx context.Context, body []byte, attemptNo int) outcome {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.endpoint, bytes.NewReader(body))
	if err != nil {
		return outcome{err: fmt.Errorf("build request: %w", withoutURL(err))}
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := n.hc.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return outcome{err: fmt.Errorf("send: %w", ctxErr)}
		}

		return outcome{err: fmt.Errorf("send: %w", withoutURL(err)), wait: backoff(attemptNo), retry: true}
	}

	defer func() { _ = resp.Body.Close() }()

	return classify(resp, attemptNo)
}

// classify turns a reply into an outcome: a 2xx with ok:true is delivered, a 429 or a 5xx is
// retried, and anything else (another 4xx, a 2xx with ok:false or an unreadable body, a 3xx)
// fails at once, since sending the same request again would fail the same way.
func classify(resp *http.Response, attemptNo int) outcome {
	var reply apiReply

	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, maxReplyBytes)).Decode(&reply)
	failure := &statusError{status: resp.StatusCode, errorCode: reply.ErrorCode}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return tooManyRequests(failure, reply.Parameters, attemptNo)
	case resp.StatusCode >= http.StatusInternalServerError:
		return outcome{err: failure, wait: backoff(attemptNo), retry: true}
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices:
		return outcome{err: failure}
	case decodeErr != nil:
		return outcome{err: fmt.Errorf("%w: decode reply: %w", failure, decodeErr)}
	case !reply.OK:
		return outcome{err: fmt.Errorf("%w: ok is false", failure)}
	default:
		return outcome{}
	}
}

// tooManyRequests honors flood control's parameters.retry_after up to the cap, and falls back to
// the ordinary backoff when the reply names no wait.
func tooManyRequests(failure *statusError, params *replyParameters, attemptNo int) outcome {
	if params == nil || params.RetryAfter <= 0 {
		return outcome{err: failure, wait: backoff(attemptNo), retry: true}
	}

	// Compared in seconds, before any conversion, so an absurd retry_after cannot overflow a
	// time.Duration into a short wait.
	if params.RetryAfter > maxRetryAfterSeconds {
		return outcome{err: fmt.Errorf(
			"%w: retry_after %ds exceeds the %ds cap", failure, params.RetryAfter, maxRetryAfterSeconds,
		)}
	}

	return outcome{err: failure, wait: time.Duration(params.RetryAfter) * time.Second, retry: true}
}

// backoff is the wait before the retry that follows attempt attemptNo (0-indexed). It has no
// jitter: one chat's handful of messages cannot stampede the Bot API.
func backoff(attemptNo int) time.Duration {
	return baseDelay << attemptNo
}

// withoutURL drops the *url.Error wrapper that http.Client puts around a transport failure: its
// text quotes the request URL, and the URL path holds the bot token.
func withoutURL(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErr.Err
	}

	return err
}
