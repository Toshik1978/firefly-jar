---

description: "Task list for Telegram forum topic and silent delivery"
---

# Tasks: Telegram Forum Topic and Silent Delivery

**Input**: Design documents from `specs/002-telegram-topics/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/config.md, quickstart.md

**Tests**: no standalone test tasks. Every task is implemented test-first (constitution III) and writes its own
tests in the package's existing testify suite (`s.Run` table cases, per-client `httpmock`, `testing/synctest`
for any retry timing). Fixtures are anonymized.

**Organization**: grouped by user story. US2 (unchanged behaviour) is a property of the US1 change and is proven
inside T002 by the byte-equality assertion.

## Format: `[ID] [P?] [Story] Description`

## Phase 1: Foundational (config keys)

**Purpose**: the two keys parse into `config.Telegram`. US1, US3 and US4 all depend on it.

- [x] T001 Add `ThreadID *int64 \`yaml:"thread_id"\`` and `Silent bool \`yaml:"silent"\`` to `Telegram` in
  internal/config/notify.go, with a why-comment (thread is the forum topic for every chat, nil = no topic;
  silent = disable_notification). Tests in internal/config/config_test.go: a config without either key loads
  with `ThreadID == nil` and `Silent == false`; `thread_id: 42` and `silent: true` load as set; `thread_id: "x"`
  and `silent: "yes"` fail loading (strict decoding).

**Checkpoint**: config carries the keys; nothing consumes them yet.

---

## Phase 2: User Story 1 - Digest lands silently in the Finance topic (Priority: P1) 🎯 MVP

**Goal**: with `thread_id` and/or `silent` set, every part to every chat carries `message_thread_id` and/or
`disable_notification: true`; with neither, the request is today's byte-for-byte (US2).

**Independent Test**: httpmock records request bodies for single- and multi-part digests under each of the four
key combinations.

- [ ] T002 [US1] In internal/notify/telegram/api.go add `MessageThreadID int64 \`json:"message_thread_id,omitempty"\``
  and `DisableNotification bool \`json:"disable_notification,omitempty"\`` to `sendMessageRequest`. In
  internal/notify/telegram/telegram.go add `type Option func(*Notifier)` with `WithThread(id int64)`,
  `WithSilent(silent bool)` and `WithLogger(*slog.Logger)` (nil keeps the default discard logger), make `New`
  variadic in `opts ...Option` (existing callers unchanged), and have `sendPart` take whether to include the
  thread. Tests in internal/notify/telegram/telegram_test.go: (a) no options → the recorded JSON body equals
  the exact bytes the previous release sent (assert the literal, not a re-marshal), for a single and a split
  digest (US2 acceptance 1); `WithSilent(false)` → same bytes (US2 acceptance 2); (b) thread + silent → every
  part to every chat has both; (c) thread only → thread, no `disable_notification`; (d) silent only → silent,
  no `message_thread_id`.
- [ ] T003 [US1] In internal/app/wire.go `newNotifiers`, pass `telegram.WithThread(*tg.ThreadID)` only when
  `tg.ThreadID != nil`, `telegram.WithSilent(tg.Silent)`, and `telegram.WithLogger(log)` (thread the run
  logger into `newNotifiers`). Test in the existing internal/app suite that drives `BuildDeps`/`RunEnv` against
  its fake Telegram server: a config with `thread_id` and `silent: true` produces a request carrying both.

**Checkpoint**: US1 and US2 done; shippable before the cutover.

---

## Phase 3: User Story 3 - A deleted topic does not lose the digest (Priority: P2)

**Goal**: "message thread not found" → resend that part once without the topic, rest of that chat's digest
without it, one WARN; closed topic and every other failure unchanged.

**Independent Test**: httpmock responders reject requests carrying `message_thread_id` with HTTP 400
`{"ok":false,"error_code":400,"description":"Bad Request: message thread not found"}` and accept others.

- [ ] T004 [US3] In internal/notify/telegram/api.go, decode `description` into a local in `classify` (not into
  `apiReply` as a stored field that outlives the call, and never into an error); on a non-2xx or `ok:false`
  reply whose description contains `message thread not found` (case-insensitive), return a new outcome flag
  `threadNotFound` (not retried, err = today's `statusError` text only). Confirm the Bot API wording with ctx7
  first. Tests: the match is case-insensitive and finds the phrase with a `Bad Request: ` prefix; a 400 with
  `Bad Request: TOPIC_CLOSED` is an ordinary non-retried failure; 429/5xx classification unchanged; no
  outcome's error text contains the description; a request carrying `message_thread_id` that gets a 429 (with
  `retry_after`) or a 5xx is retried with `message_thread_id` still in the body (spec edge case "topic kept on
  retries"; timing under `synctest`).
- [ ] T005 [US3] In internal/notify/telegram/telegram.go `sendParts`, keep a per-chat `useThread` (true when a
  thread is configured); on `threadNotFound` while `useThread`, set it false, log one WARN
  `telegram topic not found, sent to general` with `chat` = `notify.MaskRecipient(...)` and `thread_id`, and
  resend the same part through the ordinary retry loop without the thread; later parts start without it;
  `threadNotFound` on a request without a thread is an ordinary failure. Never resend an already delivered
  part. Tests (logger captured into a buffer handler): fallback on part 1 of 3 → 4 requests, part 1 twice
  (with then without topic), parts 2–3 without, exactly one WARN, WARN text contains no description and no
  digest text (US3 acceptance 1–2, SC-005); two chats, only one rejects → only that chat falls back (acceptance
  3); fallback resend gets 500s until exhausted → that chat's Result has an error, under `synctest` (acceptance
  4); closed topic → chat fails, no resend, no WARN (acceptance 6); a second `Send` call tries the topic again
  (per-run scope). Also, in the existing internal/app suite (same fake Telegram server as T003): a `check` run
  whose fake answers "message thread not found" to the topic request and accepts the resend exits by the normal
  rules (1 with missing transactions, not 2). This assertion lives here rather than in T003 because it needs the
  fallback.

**Checkpoint**: no message is lost to a missing topic.

---

## Phase 4: User Story 4 - A bad topic id is caught at startup (Priority: P3)

**Goal**: an invalid `thread_id` stops the run at validation with exit 2.

**Independent Test**: config validation tests with invalid values.

- [ ] T006 [US4] In internal/config/validate.go `validateNotify`, when `Notify.Telegram != nil` and
  `ThreadID != nil && *ThreadID <= 0`, fail with exactly `notify.telegram.thread_id: must be a positive integer`
  (keep the function within lint limits; extract a `validateTelegram` helper if needed). Tests in
  internal/config/config_test.go: `thread_id: 0` and `thread_id: -5` fail with that message; `thread_id: 1`
  passes; email validation unchanged.

**Checkpoint**: all stories done.

---

## Phase 5: Polish & Cross-Cutting Concerns

- [ ] T007 Document both keys and the fallback: add commented `thread_id:` / `silent:` lines with defaults
  to config.example.yaml; fold specs/002-telegram-topics/contracts/config.md into
  specs/001-missing-tx-reminder/contracts/config.md (sample + validation rules); mention topic, silent and the
  General fallback in the Telegram part of README.md; add one line on the fallback to the Telegram notifier in
  docs/ARCHITECTURE.md. No real ids: examples use `-1001234567890` and `42`.
- [ ] T008 Run `task check` and `task cover:check`; both must exit 0. Walk quickstart.md's automated list and
  confirm each item maps to a passing test.

---

## Dependencies & Execution Order

- T001 → T002 → T003 (wire needs the config fields and the options).
- T002 → T004 → T005 (fallback needs the thread switch in `sendPart`; T005 consumes T004's outcome).
- T001 → T006 (independent of Phases 2–3; can run after T001).
- T007 after T006; T008 last.

Story order: US1/US2 (MVP, safe to ship alone) → US3 → US4. US4 is independent of US1/US3 and may run in
parallel with Phase 2–3 in a separate subagent once T001 is in.

### Parallel Opportunities

- T006 alongside T002–T005 (different package, depends only on T001).

## Implementation Strategy

MVP: Phases 1–2 deliver topic + silent with unchanged defaults: enough for the cutover runbook. Phase 3 adds the
no-loss fallback the shared contract requires; it must land before the release that the runbook relies on.
Phase 4 hardens config. One release (next minor) carries all phases.
