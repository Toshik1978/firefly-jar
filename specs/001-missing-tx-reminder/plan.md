# Implementation Plan: Missing Transaction Reminder

**Branch**: `feature/001-missing-tx-reminder` | **Date**: 2026-09-22 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/001-missing-tx-reminder/spec.md`

## Summary

`firefly-jar` is a one-shot Go CLI, run by cron on a Linux server. It fetches recent bank transactions through
Enable Banking and the matching asset-account transactions from Firefly III, both read-only. It pairs them
deterministically (exact amount, ±N days) and sends a plain-text digest of unmatched bank transactions to
Telegram and/or email recipients. Exit codes 0, 1 and 2 separate clean, missing-found and check-failed runs.
Only the Enable Banking session is persisted. The design is stdlib-first: hand-written read-only API clients,
a GET-only transport guard for Firefly III, and a small, owner-approved set of third-party dependencies
(YAML, exact decimal arithmetic, CLI parsing). See [research.md](research.md) for every decision.

## Technical Context

**Language/Version**: Go 1.27.1 (per `go.mod`, module `github.com/Toshik1978/firefly-jar`)

**Primary Dependencies**:
- Go standard library: `net/http`, `crypto/rsa` (JWT RS256), `net/smtp` + `crypto/tls`, `log/slog`,
  `encoding/json`.
- `github.com/goccy/go-yaml` v1.19.x, strict YAML config decoding (R13).
- `github.com/shopspring/decimal` v1.4.0, exact decimal arithmetic backing `domain.Amount` (R15). Owner
  approved 2026-09-22 as an exception to the no-release-before-2025-01-01 rule.
- `github.com/spf13/cobra`, CLI parsing for `check`/`auth`/`accounts`, in place of `flag.NewFlagSet` (R19).
- `github.com/stretchr/testify` (suites), test-only (R17).
- `internal/civil` is a trimmed copy of `cloud.google.com/go/civil` v0.123.0's `Date` (Apache-2.0 header
  kept), not a dependency on `cloud.google.com/go` itself (R19).
- Considered and not adopted: `fatih/color`, `dustin/go-humanize` — the digest is plain text for
  Telegram/email, and the terminal output (`auth` prompt, `accounts` table) is too small to benefit.
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
- 10-minute run cap; 30 s per HTTP attempt (each retry gets its own 30 s).

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
| VI. Simple, Unattended Operation | One-shot binary with no scheduler or server, distinct exit codes, fail-fast config, stdlib-first with every dependency justified, slog summary. | PASS | PASS: a small set of justified runtime dependencies — YAML (R13), decimal arithmetic (R15), CLI parsing (R19) — plus a vendored copy, not a dependency, for civil dates (R19); testify is test-only (R17); oapi-codegen rejected (R1) |
| Constraints | `gofmt`, `go vet`, `go test -race` gates; exact decimals; explicit time zones. | PASS | PASS: `task check` covers all three gates (R17); `domain.Amount` (`shopspring/decimal`) and `civil.Date` (R15) |

Complexity Tracking records the deviations: TDD exceptions for scaffolding, declaration-only code and
verification-only tasks, the owner-approved dependency change, and the test-only seams.

## Project Structure

### Documentation (this feature)

```text
specs/001-missing-tx-reminder/
├── plan.md              # This file
├── research.md          # Phase 0: decisions R1–R19 (+R8a)
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
└── main.go                 # os.Exit(app.Run(os.Args[1:], …)); the cobra command tree lives in internal/app

internal/
├── domain/                 # Amount (exact decimal, shopspring/decimal), Window (built on internal/civil)
├── civil/                  # trimmed copy of cloud.google.com/go/civil's Date (no cloud.google.com/go dependency)
├── config/                 # YAML load (strict), env/file secret resolution, validation
├── logging/                # slog multi-handler (file JSON + stderr WARN+), redacting ReplaceAttr
├── redact/                 # IBAN masking, secret scrubbing for errors and URLs
├── httpx/                  # retry RoundTripper (backoff, Retry-After), per-attempt timeout RoundTripper
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
└── app/                    # cobra CLI (Run(args, stdin, stdout, stderr) int), check / auth / accounts orchestration

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
| T074, T075, T076: no RED phase observed (§III step 2). `FailFastSuite` (T074) and the US4 acceptance suite (T076) passed on arrival, and T075 ("make T074 pass") changed no production code | The fail-fast ordering, delivery fallback and per-bank isolation they pin were already built test-first by earlier tasks (T070–T073 and Phases 3–5). T074 proved each case's sensitivity by mutating the production code, seeing the case fail, and reverting | Deleting working code to stage an artificial RED would add risk and prove nothing the mutation check did not. The suites stay as regression pins |
| T081: coverage tests added after the code they cover (§III step 2) | `bank.Status.String` and several `state` Load/Save error paths had no test. T081's 80% coverage target called for tests over existing behaviour, with no production change | Rewriting the covered code to stage a RED would change nothing a reviewer could observe. The new tests pin the existing behaviour |
| Dependency set widened after planning, owner-approved 2026-09-22 (§VI, justification in Technical Context, R15, R19) | `github.com/shopspring/decimal` v1.4.0 backs `domain.Amount`, a named exception to the no-release-before-2025-01-01 rule. `github.com/spf13/cobra` parses the CLI and brings `github.com/spf13/pflag` and `github.com/inconshreveable/mousetrap` in as indirect dependencies. `internal/civil` is a trimmed copy of `cloud.google.com/go/civil`'s `Date` with its Apache-2.0 header kept. `go.yaml.in/yaml/v3` is an indirect, test-only dependency of testify | The original stdlib-plus-go-yaml plan had a hand-rolled minor-units/scale `Amount`, a hand-rolled date type and `flag.NewFlagSet` subcommands. That meant bespoke decimal normalization, rounding and date arithmetic, which well-tested libraries already provide |
| fj-xwu.15: package `app`'s `TestApp` is declared twice, in `app_test.go` (`//go:build !live`) and `app_live_test.go` (`//go:build live`, the same `suite.Run` lines plus `LiveSuite`) | `LiveSuite` must stay behind the `live` tag, out of `task check`, while each build keeps exactly one `Test<Package>` that only calls `suite.Run` (`.claude/CLAUDE.md` Testing rules 1 and 2) | A second `TestAppLive` entry point broke rule 1 under the `live` tag, and a build-tagged helper returning the suite list would put a non-`suite.Run` call in the entry point, breaking rule 2. The cost is 14 duplicated `suite.Run` lines to keep in sync |
| T077, T078: test-only seams on `app.Env` (`TelegramBaseURL`, `SMTPTLSConfig`, `SMTPAddr`, `EnableBankingBaseURL`) and `email.Notifier.WithAddr` (§VI YAGNI) | The privacy and read-only suites drive the real Telegram, email and Enable Banking clients against in-process fakes. Config validation allows only SMTP ports 587 and 465, which a fake server cannot bind | `app.Run`, the only entry point `main` uses, leaves every seam at its zero value, and no config key, env var or flag reaches them. Substituting a fake `Provider` or `Notifier` instead would leave the real clients' request bodies and headers unchecked |
