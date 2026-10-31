# Research: Telegram Forum Topic and Silent Delivery

## R1. Request fields

- **Decision**: add `message_thread_id` (integer) and `disable_notification` (boolean) to the `sendMessage`
  body, both `omitempty`. The thread id is a pointer, or a zero value meaning unset. Silent is sent only when
  true.
- **Rationale**: these are the Bot API's own `sendMessage` parameters for forum topics and silent delivery.
  `omitempty` leaves the JSON identical to today's when the keys are unset (FR-103, SC-002).
- **Alternatives**: always sending `disable_notification:false`. Rejected: it changes today's request bytes,
  for no gain.

## R2. Recognising a missing topic

- **Decision**: on a failed reply (non-2xx, or `ok:false`), decode `description` into a local string. If it
  contains `message thread not found` (case-insensitive), return the `threadNotFound` outcome: not retried,
  with an error naming only the HTTP status. `classify` does not know whether the request carried a thread id;
  the send loop treats `threadNotFound` as a fallback trigger only for a request that did, and as an ordinary
  failure otherwise.
- **Rationale**: Telegram reports a deleted or unknown topic as HTTP 400 `Bad Request: message thread not found`.
  A closed topic gives a different description (`TOPIC_CLOSED`), so it stays an ordinary 400 failure, which is
  the owner's choice. The phrase match is the narrowest check that separates the two. Before relying on the exact
  wording, the implementer confirms it with `ctx7` (Bot API docs).
- **Privacy**: the description is free text that could quote the message. It lives only in a local variable
  inside `classify`: no struct field, no error wrapping, no log.

## R3. Where the fallback state lives

- **Decision**: `sendParts` holds a `useThread` boolean per chat. It starts true when a thread id is configured.
  On `threadNotFound`, it is set false, one WARN is logged, and the same part is sent again through the ordinary
  retry loop. Later parts start without the topic. The value is local to one `Send`. A fallback resend's own
  failure follows today's rules (FR-110).
- **Rationale**: the decision is per chat (FR-107, User Story 3, scenario 3) and per run (spec Assumptions).
  Keeping it on the stack keeps `Notifier` free of state between Send calls, as its doc comment promises today.

## R4. Logger and options

- **Decision**: `telegram.New(token, chatIDs, hc, baseURL, opts ...Option)` with `WithThread(id int64)`,
  `WithSilent(bool)` and `WithLogger(*slog.Logger)`. The default logger discards output. `app.newNotifiers`
  passes the run logger, so the WARN goes through the redacting handler to the log file and stderr.
- **Rationale**: this mirrors `firefly.WithLogger`. Existing callers and tests keep compiling, and `ireturn` and
  `gochecknoglobals` are respected.
- **WARN content**: message `telegram topic not found, sent to general`, with keys `chat` (masked via
  `notify.MaskRecipient`) and `thread_id`. Nothing else.

## R5. Config typing

- **Decision**: `ThreadID *int64 \`yaml:"thread_id"\`` and `Silent bool \`yaml:"silent"\``. Validation fails when
  `ThreadID != nil && *ThreadID <= 0`, with `notify.telegram.thread_id: must be a positive integer`. A wrong type
  fails strict decoding at load. Both exit 2 through the existing config-error path.
- **Rationale**: the pointer separates unset from an explicit 0, so 0 can be rejected as invalid (FR-105).
