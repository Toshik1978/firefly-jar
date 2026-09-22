# firefly-jar Constitution

## Core Principles

### I. Read-Only Reconciliation (NON-NEGOTIABLE)

firefly-jar is a **reminder tool, not an importer**. It detects bank transactions that are missing
from Firefly III and tells the owner so they can enter them by hand.

- The application MUST NOT create, update, or delete any Firefly III resource (transactions,
  accounts, budgets, tags, rules, attachments, or anything else). The Firefly III client MUST
  expose only read operations and MUST issue only `GET` requests. A test MUST fail if any other
  HTTP method can reach the Firefly III API.
- The application MUST NOT initiate payments or any other write action at a bank. Bank
  integrations MUST be limited to account-information (read-only) scopes.
- Features that would auto-import, auto-create, or "one-click add" transactions are out of scope
  and MUST be rejected at specification time.

Rationale: the owner deliberately enters transactions manually. Firefly III personal access
tokens are not scoped, so the read-only guarantee has to be enforced in code.

### II. Never Miss a Transaction

The tool's only job is to catch omissions, so a missed transaction (false negative) is a
worse failure than a spurious reminder (false positive).

- Every bank transaction in the checked window MUST end up in exactly one explicit, counted
  state: **matched** to a Firefly III transaction, **reported** as missing, **set aside by a
  documented, deterministic rule** (e.g. a pending copy of a booked entry with the same bank
  identifier, or a cancelled/rejected entry), or on an account **reported as unchecked**.
  Transactions MUST NOT be dropped silently. Every set-aside entry is counted in the run summary.
- Matching MUST be deterministic and explainable. Given the same inputs it produces the same
  result, and for every match or non-match the tool can state which criteria were used (amount,
  currency, date tolerance, account, counterparty/description).
- Ambiguous cases, such as several candidate matches or an amount match outside the date
  tolerance, MUST be reported rather than guessed.
- Reminders for a still-missing transaction MUST continue on later runs until it is matched or
  leaves the check window. Reminders MAY be deduplicated or batched so the same item is not repeated within a
  single notification.
- Operational failures MUST be reported as loudly as missing transactions. That covers expired
  or soon-to-expire bank consent, provider or Firefly III errors, and partial data fetches.
  A run that could not check everything MUST NOT claim "nothing missing".

Rationale: a quiet run has to mean "verified complete". If it can also mean "check failed",
the tool is useless.

### III. Superpowers Workflow & Test-First (NON-NEGOTIABLE)

Implementing any task list (e.g. `tasks.md` produced by `/speckit-tasks`) MUST follow the
Superpowers workflow, in this order:

1. **Worktree**: work happens in an isolated git worktree on a feature branch
   (`superpowers:using-git-worktrees`), never directly on the default branch.
2. **TDD, red-green-refactor**: production code is written only to make a failing test
   pass (`superpowers:test-driven-development`). The test MUST be observed failing for the
   expected reason before the implementation is written.
3. **Subagent-driven execution**: tasks are executed through
   `superpowers:subagent-driven-development`, with independent tasks dispatched in parallel
   where they share no state.
4. **Code review**: completed work is reviewed via `superpowers:requesting-code-review`, and
   feedback is handled per `superpowers:receiving-code-review` before integration.
5. **Finish branch**: integration (merge/PR/cleanup) goes through
   `superpowers:finishing-a-development-branch`, with the full test suite passing.

Skipping or reordering these steps requires an explicit, recorded justification in the
feature's `plan.md` Complexity Tracking section.

Rationale: reconciliation logic is subtle and failures are silent by nature. Test-first and
independent review are the main defenses against Principle II violations.

### IV. Provider-Agnostic Bank Integration

- The reconciliation core MUST depend only on a bank-provider interface and a normalized
  transaction model. It MUST NOT depend on Enable Banking (the current provider) or on any
  future provider directly.
- Each provider adapter MUST carry contract tests against recorded or fixture responses. Unit
  and contract tests MUST NOT call live bank or Firefly III APIs. Live checks, if any, MUST be
  opt-in and excluded from the default `go test ./...` run.
- Provider-specific concerns (auth, consent/session lifecycle, pagination, rate limits, pending
  vs. booked status) MUST be handled inside the adapter and surfaced to the core in normalized
  form, including consent expiry dates for Principle II alerts.

Rationale: open-banking aggregators change and banks drop out. Swapping or adding a provider
MUST NOT require touching matching logic.

### V. Secrets & Financial Data Privacy

- Credentials (Firefly III token, Enable Banking application ID and private key, session and
  consent identifiers, notification channel secrets) MUST come from environment variables or
  mounted secret files. They MUST NOT be committed, hard-coded, or written to logs, errors, or
  notifications.
- Logs MUST NOT contain full account numbers/IBANs, full counterparty details, or raw provider
  payloads. Identifiers MUST be masked or truncated. Notifications MUST contain only what the
  owner needs to find and enter the transaction (date, amount, currency, account alias or masked
  identifier, short description, and a masked hint to a possibly matching Firefly III entry).
- Any persisted state (e.g. reminder history) MUST store only what reconciliation needs and
  MUST live in a location with owner-only file permissions.
- Test fixtures MUST use synthetic or fully anonymized data.

Rationale: the tool handles a household's complete financial history, and leaking it is
worse than a missed reminder.

### VI. Simple, Unattended Operation

- The tool MUST build as a single static Go binary that performs one check per invocation and
  exits. Scheduling is delegated to the host (cron on a Linux server). The tool MUST NOT
  contain its own scheduler, daemon mode, or HTTP server.
- A run MUST exit `0` when everything is verified, and with a distinct non-zero code
  when missing transactions were found or the check itself failed, so schedulers can alert on
  exit codes.
- Configuration MUST be explicit (file and/or env), validated at startup, and fail fast with a
  clear message.
- The Go standard library is preferred. Each third-party dependency MUST be justified in the
  feature's `plan.md`. No speculative abstractions beyond the provider interface of
  Principle IV (YAGNI).
- Logging MUST be structured (`log/slog`), and each run MUST emit a summary: accounts checked,
  window, matched / missing / set-aside / unchecked counts, and errors.

Rationale: the tool runs unattended for months, and the owner has to be able to trust it
without babysitting it.

## Technology & Operational Constraints

- **Language/toolchain**: Go, at the version declared in `go.mod` (currently 1.27.1).
  Module path: `github.com/Toshik1978/firefly-jar`.
- **Integrations**: Firefly III REST API (read-only, per Principle I). Enable Banking
  (account-information only) is the initial bank provider.
- **Quality gates**: `task check` MUST exit 0 before every commit and before a branch is finished. It runs
  golangci-lint formatting (gofumpt, a superset of `gofmt`), the lint suite (including `go vet`), and
  `go test -race ./...`.
- **Time and money**: amounts MUST be handled as exact decimals or integer minor units, never
  floating point. Dates MUST be compared with explicit time zones and a configurable matching
  tolerance.
- **Deployment**: a host binary on a Linux server, invoked by cron (`flock` is recommended to
  prevent overlapping runs). Cron frequency MUST respect PSD2 unattended-access limits
  (typically ≤ 4 fetches per account per day).

## Development Workflow

- Features are specified and planned through Spec Kit (`/speckit-specify` → `/speckit-clarify`
  as needed → `/speckit-plan` → `/speckit-tasks`) and implemented per Principle III.
- Every `plan.md` MUST include a Constitution Check that confirms Principles I–VI. A read-only
  violation (Principle I) cannot be justified and blocks the plan.
- Every change to matching or reconciliation logic MUST add or update tests for the missed,
  set-aside, and unchecked paths, not only the happy "matched" path.
- Code review (Principle III, step 4) MUST explicitly check for Firefly III write paths,
  secret or PII leakage in logs, and silent-drop paths in reconciliation.

## Governance

- This constitution supersedes all other project practices and guidance. When a spec, plan,
  task list, or review conflicts with it, the constitution wins until it is amended.
- **Amendments**: proposed via `/speckit-constitution`. Each amendment MUST update the version
  and Last Amended date and include a Sync Impact Report for review. The report is removed
  before the amended file is committed.
- **Versioning** (semantic):
  - MAJOR: a principle is removed or redefined incompatibly, e.g. relaxing Principle I.
  - MINOR: a principle or section is added, or guidance is materially expanded.
  - PATCH: clarifications and wording fixes with no semantic change.
- **Compliance**: every plan's Constitution Check and every code review MUST verify compliance.
  Any added complexity MUST be justified in the plan's Complexity Tracking table.

**Version**: 1.0.0 | **Ratified**: 2026-09-22 | **Last Amended**: 2026-09-22
