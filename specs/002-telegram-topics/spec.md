# Feature Specification: Telegram Forum Topic and Silent Delivery

**Feature Branch**: `feature/002-telegram-topics`

**Created**: 2026-10-31

**Status**: Draft

**Input**: User description: "Telegram forum topic and silent delivery for the digest notifier. Add two optional
keys under notify.telegram: `thread_id` (positive integer, the forum topic's message_thread_id, applied to every
chat in chat_ids) and `silent` (bool, default false, sends with disable_notification=true). With neither key set,
the sendMessage request is byte-for-byte today's behaviour. A thread_id that is not a positive integer fails config
validation (exit 2). Silence is opt-in and is not implied by thread_id. If Telegram rejects a send because the
message thread was not found, resend that part once without the thread and log one WARN; the remaining parts of a
split digest for that chat also go without it. The Bot API description is matched only against that fixed phrase
and is never logged or echoed. Any other failure keeps today's retry/classification rules. Unset options must ship
safely before the cutover."

## Context

The owner's notifications are moving from one Telegram channel to a single forum supergroup whose topics group
messages by category. The firefly-jar digest belongs in the group's "Finance" topic and must not make the phone
ring: it is a routine reminder, not an alert. The shared contract across the owner's senders is: send to the
configured topic, choose loud or silent per sender, and never lose a message to routing. A message whose topic
cannot be used lands in the group's General topic instead.

This feature extends FR-023 and FR-027/FR-028 of `001-missing-tx-reminder` for the Telegram channel only. Email is
unchanged.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Digest lands silently in the Finance topic (Priority: P1)

The owner configures the Telegram channel with the supergroup's chat id, the Finance topic's id and silent
delivery. When a run finds missing transactions, the digest appears in the Finance topic without a notification
sound.

**Why this priority**: this is the reason for the feature. Without it the digest lands in General and buzzes the
phone.

**Independent Test**: run `check` against a fake Telegram endpoint with `thread_id` and `silent: true` configured,
and inspect the recorded send requests.

**Acceptance Scenarios**:

1. **Given** `thread_id: 42` and `silent: true`, **When** a digest is sent, **Then** every message to every chat
   in `chat_ids` carries topic 42 and is marked silent.
2. **Given** `thread_id: 42` and `silent` unset, **When** a digest is sent, **Then** every message carries topic
   42 and is sent with sound.
3. **Given** `silent: true` and `thread_id` unset, **When** a digest is sent, **Then** every message is silent and
   carries no topic.
4. **Given** a digest split into several messages, **When** it is sent, **Then** every part carries the same
   topic and silent setting.

---

### User Story 2 - Existing configurations behave exactly as before (Priority: P1)

The owner upgrades firefly-jar before the new group exists. The config has neither new key, and the digest still
goes to the old channel exactly as it does today.

**Why this priority**: the release has to ship before the cutover. Any change in behaviour for an unchanged config
is a regression.

**Independent Test**: with a config that has neither key, the recorded send requests are identical to those of
the previous release.

**Acceptance Scenarios**:

1. **Given** neither `thread_id` nor `silent` is set, **When** a digest is sent, **Then** the request carries no
   topic field and no silent field, and is otherwise unchanged.
2. **Given** `silent: false` set explicitly, **When** a digest is sent, **Then** the request carries no silent
   field.

---

### User Story 3 - A deleted topic does not lose the digest (Priority: P2)

The Finance topic is deleted or its id is wrong. Telegram rejects the send because the message thread was not
found. firefly-jar resends without the topic, so the digest lands in General, and logs a warning, which cron
mails to the owner.

**Why this priority**: the contract says no message is ever lost to routing. A misconfigured topic must degrade
to General, not to a failed delivery.

**Independent Test**: the fake Telegram endpoint rejects the first request carrying a topic with "message thread
not found" and accepts requests without one.

**Acceptance Scenarios**:

1. **Given** Telegram answers "message thread not found" to a message that carries a topic, **When** the digest
   is sent, **Then** that message is resent once without the topic, still with the configured silent setting,
   and one warning is logged for that chat.
2. **Given** a digest of several parts whose first part hit "message thread not found", **When** the remaining
   parts are sent to the same chat, **Then** they go without the topic, and no further warnings are logged for
   that chat.
3. **Given** two chats where only one rejects the topic, **When** the digest is sent, **Then** only that chat
   falls back to General; the other keeps the topic.
4. **Given** the resend without the topic also fails, **When** delivery is evaluated, **Then** that chat counts
   as failed under today's rules (FR-028). No further resend happens.
5. **Given** Telegram rejects a send for any other reason, **When** delivery is evaluated, **Then** today's retry
   and failure rules apply unchanged and no topic fallback happens.
6. **Given** the configured topic exists but is closed, **When** the digest is sent, **Then** Telegram's
   rejection is handled as any other failure: no fallback to General, the chat counts as failed (FR-028), so the
   owner notices and reopens the topic.

---

### User Story 4 - A bad topic id is caught at startup (Priority: P3)

The owner mistypes the topic id (zero, negative, or not a number). The run stops with a configuration error
instead of sending anywhere, and cron mails the error.

**Why this priority**: configuration is validated at startup (constitution VI). A loud config error is preferable
to a guess, and it is still not silent.

**Independent Test**: load configs with invalid `thread_id` values and check the validation error and exit
status.

**Acceptance Scenarios**:

1. **Given** `thread_id: 0` or a negative value, **When** the config is validated, **Then** validation fails with
   a message naming `notify.telegram.thread_id`, and the run exits 2.
2. **Given** `thread_id: "finance"` or `silent: "yes"`, **When** the config is loaded, **Then** loading fails and
   the run exits 2.

---

### Edge Cases

- **Closed topic**: fails loudly by design (owner's decision). Only a topic that is not found falls back to
  General; a closed topic is a deliberate state the owner should notice, not a routing accident.
- **Topic set on a chat that is not a forum**: Telegram's answer decides. If it reports "message thread not
  found", the fallback applies. If it accepts the message, it is delivered.
- **Rate limiting or server errors on a message with a topic**: today's retry rules apply. The topic is kept on
  retries, because only the "thread not found" rejection triggers the fallback.
- **The fallback resend is rate limited**: it follows today's retry rules like any other send.
- **Error text varies in case or carries a prefix** (e.g. "Bad Request: message thread not found"): the match is
  case-insensitive and finds the phrase anywhere in the error text.
- **Telegram's error text could echo message content**: it is never logged, stored or echoed, only compared with
  the fixed phrase.
- **`check --stdout`**: unaffected, since nothing is sent to Telegram.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-101**: The Telegram channel configuration MUST accept an optional `thread_id`, the forum topic id that
  digest messages are posted to. It applies to every chat in `chat_ids`.
- **FR-102**: The Telegram channel configuration MUST accept an optional `silent` flag, default false. When true,
  every digest message is sent without a notification sound.
- **FR-103**: When `thread_id` is unset, messages MUST carry no topic. When `silent` is unset or false, messages
  MUST carry no silent marker. With neither set, the request content MUST equal that of the previous release.
- **FR-104**: `silent` and `thread_id` MUST be independent. Setting one MUST NOT imply the other.
- **FR-105**: A `thread_id` that is not a positive integer MUST fail configuration validation, naming the key, and
  the run MUST end with exit status 2. Values of the wrong type for either key MUST fail loading.
- **FR-106**: When Telegram rejects a message that carries a topic and the error says the message thread was not
  found (case-insensitive match of the phrase "message thread not found"), the notifier MUST resend that message
  once without the topic and with the same silent setting.
- **FR-107**: After the fallback in FR-106, the remaining messages of the same digest to the same chat MUST be
  sent without the topic. Other chats are unaffected.
- **FR-108**: Each fallback MUST log exactly one WARN per affected chat. The warning names the masked chat and
  the configured topic id and says the digest went to General. It MUST NOT contain Telegram's error text or any
  digest content.
- **FR-109**: Telegram's error text MUST only be compared with the fixed phrase. It MUST NOT be logged, stored,
  returned in errors or echoed anywhere.
- **FR-110**: Every other failure, including a closed topic and a failure of the fallback resend itself, MUST
  follow today's retry, classification and per-recipient failure rules (FR-028, FR-029).
- **FR-112**: A part already delivered MUST NOT be sent again because of a later fallback.
- **FR-111**: The configuration contract (`contracts/`) and the sample configuration MUST document both keys,
  their defaults and the fallback behaviour.

### Key Entities

- **Telegram channel settings**: bot token source, list of chat ids, optional topic id, silent flag.
- **Per-chat delivery state**: whether the topic is still used for the current digest to that chat. It starts as
  "use topic if configured" and switches off for the rest of the run after a "thread not found" fallback. It is
  not persisted between runs.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With `thread_id` and `silent: true` configured, 100% of digest messages land in the configured topic
  without a notification sound.
- **SC-002**: With neither key configured, the send requests are identical to the previous release's in 100% of
  tested digests (single and split).
- **SC-003**: When the configured topic does not exist, 100% of digest messages are still delivered (to General),
  and the owner gets exactly one warning per affected chat per run.
- **SC-004**: An invalid `thread_id` never results in a send: the run stops at startup with exit status 2 in 100%
  of tested invalid values.
- **SC-005**: No Telegram error text or digest content appears in any log line produced by the fallback.

## Assumptions

- One topic id for all chats is enough. After the cutover `chat_ids` holds only the new supergroup (owner's
  confirmation). A per-chat topic is out of this feature and can be added later without breaking this shape.
- The topic id is not a secret. It sits in the config file in plain text, like the chat ids.
- The fallback decision lasts for one run only. The next run tries the topic again, so a recreated topic or a
  fixed config takes effect without any state.
- The fallback warning reaches the owner through cron mail (WARN goes to stderr, invariant 4). A run where the
  fallback succeeded still exits by today's rules (0 or 1 by result). The warning makes it non-quiet, which is
  intended.
- Validation is strict by design (constitution VI), so an invalid topic id fails the run instead of falling back
  to General. The owner accepted this difference.
- The release that carries this feature is the next minor version. Tagging is the owner's act.
