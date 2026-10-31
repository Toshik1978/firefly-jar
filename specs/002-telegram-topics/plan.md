# Implementation Plan: Telegram Forum Topic and Silent Delivery

**Branch**: `feature/002-telegram-topics` | **Date**: 2026-10-31 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/002-telegram-topics/spec.md`

## Summary

Two optional keys under `notify.telegram`: `thread_id` (forum topic) and `silent` (no notification sound). The
Telegram notifier adds `message_thread_id` and `disable_notification` to `sendMessage` only when they are set,
so an unchanged config produces today's request byte-for-byte. When Telegram answers that the message thread was
not found, the notifier resends that part once without the topic, keeps going without it for the rest of that
chat's digest, and logs one WARN. Everything stays inside `internal/notify/telegram`, plus config parsing,
validation, wiring and docs.

## Approaches

Chosen by the owner during brainstorming:

1. **Fallback inside the Telegram notifier (chosen).** `classify` reads the Bot API `description` into a local
   variable, matches it case-insensitively against `message thread not found` on a failed reply, and discards it.
   A match yields a new non-retryable outcome. The per-chat send loop turns off the topic for that chat, resends
   the failed part once without it, logs one WARN, and continues. Trade-off: the notifier gains a small piece of
   per-chat state and a logger. In return, nothing is duplicated, nothing outside the notifier knows about
   topics, and the description never leaves `classify`.
2. **Caller re-sends the whole digest without the topic (rejected).** This keeps the notifier simpler, but parts
   already delivered would arrive twice, and a Telegram-specific error would leak into `app`.
3. **Pre-check the topic through the Bot API (rejected).** The Bot API has no read-only topic lookup. Its topic
   methods create, edit or close, which adds write calls and one extra request to every run.

## Technical Context

**Language/Version**: Go 1.27.1 (`go.mod`)

**Primary Dependencies**: none new. Standard library (`encoding/json`, `strings`, `log/slog`), with existing
`goccy/go-yaml` for config and `testify` and `httpmock` in tests.

**Storage**: N/A. The fallback is per run and is not persisted.

**Testing**: `go test -race ./...` with testify suites. Notifier tests use per-client `httpmock` transports, and
retry timing tests use `testing/synctest`. Config tests use the existing YAML fixtures in the config suite.

**Target Platform**: Linux server, cron, static binary.

**Project Type**: CLI.

**Performance Goals**: N/A. At most one extra request per chat per run, and only when a topic is missing.

**Constraints**: invariant 4 (quiet when clean): the fallback WARN reaches stderr by design. Telegram's
`description` is never logged (constitution V). Lint rules: no globals, wrapped errors, functions within the
size limits, at most 5 exported types per file.

**Scale/Scope**: one package changed in substance (`internal/notify/telegram`), plus small edits in
`internal/config` and `internal/app`, and docs.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Status | Note |
|---|---|---|
| I. Read-only reconciliation | Pass | Firefly III and bank access are untouched. `sendMessage` is the only Telegram call, as today. Rejecting approach 3 avoids Telegram topic write calls too. |
| II. Never miss a transaction | Pass | Reconciliation is untouched. Delivery is more robust: a missing topic degrades to General instead of failing. Delivery failures still follow FR-028 and FR-029. |
| III. Superpowers & test-first | Pass | Executed through using-spec-kit: import, worktree, TDD, subagent-driven execution, review, finish. |
| IV. Provider-agnostic bank | N/A | No bank code touched. |
| V. Secrets & privacy | Pass | The thread id is not secret. `description` is matched and discarded, never logged or wrapped into errors. The WARN carries the masked chat (`notify.MaskRecipient`) and the topic id only. |
| VI. Simple, unattended | Pass | No new dependency. The keys are validated at startup and fail fast (exit 2). |

Post-design re-check (after Phase 1): unchanged, all pass.

## Project Structure

### Documentation (this feature)

```text
specs/002-telegram-topics/
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   └── config.md        # delta to 001's contracts/config.md (notify.telegram keys)
└── tasks.md             # /speckit-tasks
```

### Source Code (repository root)

```text
internal/config/
├── notify.go            # Telegram gains ThreadID *int64, Silent bool
├── validate.go          # validateNotify: thread_id > 0 when set
└── config_test.go       # parse + validation cases
internal/notify/telegram/
├── telegram.go          # options (WithThread, WithSilent, WithLogger); per-chat topic switch; fallback + WARN
├── api.go               # request fields (omitempty); description matched in classify; threadNotFound outcome
└── telegram_test.go     # request shape, fallback, multi-part, multi-chat, closed topic, no-leak
internal/app/
└── wire.go              # newNotifiers passes options and the run logger
config.example.yaml      # documents both keys (commented)
README.md                # notify.telegram section mentions topic + silent
docs/ARCHITECTURE.md     # one line on the Telegram fallback, if the notifier is described there
specs/001-missing-tx-reminder/contracts/config.md  # sample + rules updated (the living config contract)
```

**Structure Decision**: existing single-module layout. `telegram.New` keeps its signature and gains variadic
functional options, the same pattern as `firefly.New(..., firefly.WithLogger(log))`, so existing call sites and
tests compile unchanged.

## Complexity Tracking

| Deviation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| Brainstorming ran after `/speckit-specify` instead of before it | The spec was drafted straight from the agreed contract. The owner asked for brainstorming afterwards, and it fed decisions (closed topic, one shared topic) back into `spec.md` | Re-running specify from scratch would only reproduce the same file |
| `classify` reads the Bot API `description` the notifier previously ignored on purpose | FR-106 needs to recognise "message thread not found", and the `error_code` (400) alone is shared with every other bad request | Matching on 400 alone would also send closed topics and malformed requests to General, against the owner's decision |
