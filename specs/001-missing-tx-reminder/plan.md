# Implementation Plan: Missing Transaction Reminder

**Branch**: `feature/001-missing-tx-reminder` | **Date**: 2026-09-22 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/001-missing-tx-reminder/spec.md`

## Summary

`firefly-jar` is a one-shot Go CLI, run by cron on a Linux server. It fetches recent bank transactions through
Enable Banking and the matching asset-account transactions from Firefly III, both read-only. It pairs them
deterministically (exact amount, ±N days) and sends a plain-text digest of unmatched bank transactions to
Telegram and/or email recipients. Exit codes 0, 1 and 2 separate clean, missing-found and check-failed runs.
Only the Enable Banking session is persisted. The design is stdlib-first: hand-written read-only API clients,
a GET-only transport guard for Firefly III, and one YAML dependency. See [research.md](research.md) for every
decision.

## Technical Context

**Language/Version**: Go 1.27.1 (per `go.mod`, module `github.com/Toshik1978/firefly-jar`)

**Primary Dependencies**:
- Go standard library: `net/http`, `crypto/rsa` (JWT RS256), `net/smtp` + `crypto/tls`, `log/slog`,
  `encoding/json`.
- `github.com/goccy/go-yaml` v1.19.x, the only third-party runtime dependency (R13).
- `github.com/stretchr/testify` (suites), test-only (R17).
- Tooling (not linked into the binary): go-task, mise, golangci-lint v2, pre-commit, govulncheck.

**Storage**: One JSON state file (Enable Banking sessions, `0600`, atomic writes). No database. See
[contracts/state.md](contracts/state.md).

**Testing**:
- `task test` (`go test -race ./...`) with testify suites (one `Test<Package>` entry point per package) and
  table-driven `s.Run` subtests, `httptest` servers with anonymized fixtures, `testing/synctest`
  for retry timing, and golden files for the digest.
- An optional `live` build tag for smoke tests.

**Target Platform**: Linux server (amd64/arm64), single static binary (`CGO_ENABLED=0`), invoked by cron.

**Project Type**: CLI (single project)

**Performance Goals**: A check of up to 10 accounts and 30 days finishes in under 2 minutes (SC-008). The run is
dominated by provider latency, so accounts are fetched sequentially per bank to respect PSD2 limits.

**Constraints**:
- Zero write calls to Firefly III (FR-001).
- About 4 fetches per account per day at the bank (R7).
- No terminal output on clean or missing-found runs (FR-039).
- Money is never handled as float (FR-007).
- 10-minute run cap; 30 s per request.

**Scale/Scope**: Single user, 1–5 banks, ≤ 20 accounts, ≤ a few hundred transactions per window.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Gate | Pre-research | Post-design |
|---|---|---|---|
| I. Read-Only Reconciliation | Firefly client exposes only reads. The transport rejects non-GET, and a test proves it. Bank access is AIS only, with no payment endpoints. No import features. | PASS | PASS: R8 guard transport (no redirects followed), a client with only 2 list methods, AIS-only `POST /auth` (R3) |
| II. Never Miss a Transaction | Every bank transaction ends matched, missing, deduplicated, void or unchecked. Deterministic matching. Ambiguity is reported, not guessed. Partial runs never look clean. Consent problems are loud. | PASS | PASS: data-model invariant, exit-code rules, `NOT_AUTHORIZED`/`EXPIRED` states, void status mapping (R5), possible-match hints for competing or near-miss candidates (FR-025a, R18) |
| III. Superpowers Workflow & Test-First | Tasks are executed via worktree, TDD, subagent-driven development, code review, finish-branch. | PASS | PASS: tasks.md will be ordered test-first. **Precondition**: the repository must be `git init`-ed before the worktree step. |
| IV. Provider-Agnostic Bank Integration | The core depends on a `bank.Provider` interface and normalized types. Contract tests use fixtures, with no live calls by default. | PASS | PASS: `bank.Provider` and `bank.Authorizer` interfaces, so `check` and `auth` never import the adapter; `internal/bank/enablebanking` implements both; the `live` tag is opt-in |
| V. Secrets & Privacy | Secrets come only from env or `*_file`. Redaction in logs and notifications. IBAN masking. `0600` state. Anonymized fixtures. | PASS | PASS: config contract, slog `ReplaceAttr` redactor (R14), masked identifiers in the digest contract |
| VI. Simple, Unattended Operation | One-shot binary with no scheduler or server, distinct exit codes, fail-fast config, stdlib-first with every dependency justified, slog summary. | PASS | PASS: one runtime dependency (YAML, justified in R13); testify is test-only (R17); oapi-codegen rejected (R1) |
| Constraints | `gofmt`, `go vet`, `go test -race` gates; exact decimals; explicit time zones. | PASS | PASS: `task check` covers all three gates (R17); `domain.Amount` and `domain.Date` (R15) |

Two recorded TDD deviations for scaffolding and declaration-only code; see Complexity Tracking.

## Project Structure

### Documentation (this feature)

```text
specs/001-missing-tx-reminder/
├── plan.md              # This file
├── research.md          # Phase 0: decisions R1–R18 (+R8a)
├── data-model.md        # Phase 1: domain types, mapping, reconcile algorithm, exit rules
├── quickstart.md        # Phase 1: validation and run guide
├── contracts/
│   ├── cli.md           # commands, flags, output, exit codes
│   ├── config.md        # YAML schema and validation
│   ├── digest.md        # reminder text format, Telegram splitting, email
│   └── state.md         # persisted session file
├── checklists/requirements.md
└── tasks.md             # Phase 2 (/speckit-tasks; not created here)
```

### Source Code (repository root)

```text
cmd/firefly-jar/
└── main.go                 # flag parsing, subcommand dispatch, os.Exit(code)

internal/
├── domain/                 # Date, Amount, Window (exact decimal and civil date)
├── config/                 # YAML load (strict), env/file secret resolution, validation
├── logging/                # slog multi-handler (file JSON + stderr WARN+), redacting ReplaceAttr
├── redact/                 # IBAN masking, secret scrubbing for errors and URLs
├── httpx/                  # retry RoundTripper (backoff, Retry-After), timeouts
├── state/                  # state file model, atomic 0600 write, load and validate
├── bank/                   # Provider and Authorizer interfaces, BankAccount, BankTransaction, error kinds
│   └── enablebanking/      # JWT signer, auth flow, accounts/transactions client, normalization
├── firefly/                # Account and Entry types (foundational), GET-only transport guard, client (ListAccounts, ListAccountTransactions), page merge, split→entry
├── mapping/                # bank ↔ Firefly account resolution (IBAN+currency, overrides)
├── reconcile/              # pure matching: dedup, window, void, greedy pairing
├── report/                 # RunReport, delivery results, exit-status and digest-needed rules
├── digest/                 # RunReport → plain-text digest (golden-file tested)
├── notify/                 # Notifier interface, fan-out, delivery results
│   ├── telegram/           # sendMessage, UTF-16-aware splitting, 429 handling
│   └── email/              # net/smtp STARTTLS/implicit TLS, RFC 5322 message
└── app/                    # CLI dispatch (Run(args, stdin, stdout, stderr) int), check / auth / accounts orchestration

testdata/                   # anonymized fixtures (enablebanking/*.json, firefly/*.json), golden digests

Taskfile.yml                # setup, format, format:check, lint, test, build, check, audit, clean
.mise.toml                  # pinned go, golangci-lint, git-cliff
.golangci.yml               # ALREADY PRESENT: the author's standard lint config, verbatim (module path adapted); do not edit
.pre-commit-config.yaml     # commit-msg: conventional; pre-commit: fmt --diff; pre-push: task check
.github/workflows/          # ci.yml (task check + govulncheck), commit-lint.yml
.gitignore                  # binary, cover.out, local config and secrets
```

**Structure Decision**: A single Go module with a single binary. `internal/` keeps every package private, and
dependencies point inward: `app` uses everything; `reconcile`, `mapping` and `digest` depend only on `domain`,
`bank` types and `firefly` types; adapters depend on `domain`, `httpx` and `redact`. Tests sit next to their
packages (`_test.go`). Shared fixtures live in `testdata/`.

## Implementation Notes for Tasks

- **Suggested build order**: repository tooling (Taskfile, mise, pre-commit, CI; `.golangci.yml` already exists) so `task check`
  runs green on an empty module, then `domain`, then `redact`, `config`, `state`, then `reconcile` and `mapping` (pure
  logic, the most tests), then `firefly`, `bank/enablebanking` and `httpx`, then `digest`, `notify/*`, then
  `app`/`cmd`. Every step is red-green-refactor (Principle III).
- **User story slices**:
  - US1 (P1): check pipeline plus one notifier.
  - US2 (P2): `auth` and consent states.
  - US3 (P3): overrides, ambiguity and the `accounts` command.
  - US4 (P4): retries, per-account isolation and delivery fallback.
- **Before `/speckit-implement`**: run `git init` and commit the Spec Kit artifacts, so the Superpowers worktree
  step can branch `feature/001-missing-tx-reminder`.
- Repository conventions (testing rules, lint consequences, commit and branch rules) are in `.claude/CLAUDE.md`.

## Complexity Tracking

| Deviation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| T006: `main.go` and `cli.go` stub written without a failing test first (§III step 2) | Scaffolding so `task check` has a buildable module before any test can compile. It has no behavior beyond "print usage, exit 2". | Writing `CLISuite` first needs the testify dependency and suite layout from Phase 2. The stub's behavior is pinned by `CLISuite` in T053 before any real logic lands. |
| T025: `internal/firefly/types.go` declared without its own test (§III step 2) | Declarations only (`Account`, `Entry`) with no methods or behavior. It unblocks `mapping` and `reconcile` to run in parallel with the Firefly client. | A test of plain struct fields asserts nothing. The types are exercised test-first by T034, T035, T037 and T039. |
