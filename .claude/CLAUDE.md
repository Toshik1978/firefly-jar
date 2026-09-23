# firefly-jar: Agent Guide

This guide is for an agent working *on* this repository, not for a user of `firefly-jar`.

`firefly-jar` is a one-shot Go CLI that cron runs on a Linux server. It fetches recent bank transactions
through a bank-data provider (Enable Banking), compares them with a self-hosted Firefly III instance, and
sends a reminder digest (Telegram, email) listing the bank transactions nobody entered. It is a **reminder,
not an importer**: the owner enters every transaction by hand.

## Sources of truth

| To understand… | Read |
|---|---|
| Non-negotiable principles and governance | [`.specify/memory/constitution.md`](../.specify/memory/constitution.md) |
| What the feature must do | [`specs/001-missing-tx-reminder/spec.md`](../specs/001-missing-tx-reminder/spec.md) |
| How it is built, package layout | [`specs/001-missing-tx-reminder/plan.md`](../specs/001-missing-tx-reminder/plan.md) |
| Why each technical decision was made | [`specs/001-missing-tx-reminder/research.md`](../specs/001-missing-tx-reminder/research.md) |
| Domain types, matching algorithm, exit rules | [`specs/001-missing-tx-reminder/data-model.md`](../specs/001-missing-tx-reminder/data-model.md) |
| CLI, config, digest and state formats | [`specs/001-missing-tx-reminder/contracts/`](../specs/001-missing-tx-reminder/contracts/) |
| End-to-end validation and failure drills | [`specs/001-missing-tx-reminder/quickstart.md`](../specs/001-missing-tx-reminder/quickstart.md) |

The constitution wins every conflict. If a change needs to break it, stop and ask. Do not work around it.

## Invariants (enforced by tests; out of scope to relax)

1. **Read-only against Firefly III.** The Firefly client exposes list methods only. Its transport rejects every
   method except `GET` before the request leaves the process, and a test proves that `POST`/`PUT`/`PATCH`/
   `DELETE`, `HEAD`, `OPTIONS` and any other method never reach the server. No feature may create, edit or
   delete Firefly III data, including "helpful" auto-add or one-click import.
2. **Read-only at the bank.** Account-information consent only. No payment endpoints, and no `Psu-*` headers
   (the tool runs unattended and must not claim otherwise).
3. **Nothing is dropped silently.** Every bank transaction in the window ends up matched, missing,
   deduplicated, void, or on an unchecked account. The reconcile tests assert that invariant. A partial run
   never exits 0 or 1.
4. **Quiet when clean.** A `check` that exits 0 or 1 writes nothing to stdout or stderr. Only WARN and above
   reach stderr, because cron mails any output. The full log goes to the configured log file.
5. **No scheduler, no daemon, no HTTP server.** One invocation is one run. Cron does the scheduling.

## The gate

`task check` runs `format:check`, then `lint`, then `test`, and must exit 0 before every commit. CI runs the
same checks.

| Task | Does |
|---|---|
| `task setup` | `mise install` (pinned Go and golangci-lint), `go mod download` |
| `task format` / `task format:check` | `golangci-lint fmt` (gofumpt, gci, golines) / the same with `--diff` |
| `task lint` | `golangci-lint run` |
| `task test` | `go test -race ./...`. Never calls a live service. |
| `task build` | static binary (`CGO_ENABLED=0`, `-trimpath`) from `./cmd/firefly-jar` |
| `task audit` | `govulncheck ./...` |
| `task cover` | `go test ./... -coverpkg=./... -coverprofile=cover.out` |

Live smoke tests exist only behind the `live` build tag (`go test -tags live …`) and need a real config. They
are never part of `task check`.

`pre-commit install` wires `commit-msg` (Conventional Commits), `pre-commit` (`golangci-lint fmt --diff`) and
`pre-push` (`task check`). Never pass `--no-verify`.

## Dependencies

Standard library first. Approved direct dependencies:

- `github.com/avast/retry-go/v5`: the attempt loop and the wait between attempts inside
  `httpclient.RetryTransport`. It is a generic retry loop, not an HTTP client. Every HTTP rule stays in our wrapper
  (research R12): what is retried, the delay and jitter, the Retry-After cap, draining, and handing back the
  last response.
- `github.com/goccy/go-yaml`: strict (`DisallowUnknownField`) config decoding. The stdlib has no YAML.
- `github.com/shopspring/decimal` v1.4.0: exact decimal arithmetic backing `money.Amount`, so money is
  never a float and never a hand-rolled minor-units/scale representation.
- `github.com/spf13/cobra`: CLI parsing for `check`/`auth`/`accounts` and their flags, in place of
  `flag.NewFlagSet`.
- `github.com/stretchr/testify`: test suites only. It is never imported by non-test code.
- `github.com/jarcoal/httpmock`: test-only, per-client `http.RoundTripper` mocking for HTTP adapter tests
  (research R17, owner-approved for every test where it fits). It is never imported by non-test code. Always
  per-client (`httpmock.NewMockTransport()` injected into the client under test), never
  `httpmock.Activate`/the global `http.DefaultTransport`, so tests stay parallel-safe. Its own `go.mod`
  pulls in `github.com/maxatome/go-testdeep` and `github.com/davecgh/go-spew` for httpmock's own tests;
  those land in `go.sum` for module-graph verification but nothing from them is compiled into this
  module's build or test binary.

`internal/civil` is a trimmed copy of `cloud.google.com/go/civil`'s `Date` type (Apache-2.0 header kept),
used for civil dates end to end. It is not a dependency on `cloud.google.com/go` itself. `date.go` is that
copy; `range.go` (`civil.Range`, the check window) is this repository's own code.

Considered and not adopted: `fatih/color`, `dustin/go-humanize`. The digest is plain text for
Telegram/email, and the terminal output (`auth` prompt, `accounts` table) is too small to benefit; revisit
if colored terminal output is wanted.

Any other direct dependency needs explicit approval. State the package, what it solves, and why the standard
library is not enough, and do not add it until approved. There is no JWT library (RS256 is `crypto/rsa`), no
HTTP client library (`hashicorp/go-retryablehttp` was rejected, research R12), and no OpenAPI code
generation. API models are small hand-written structs covering only the fields in use. No release older than
2025-01-01, and no pseudo-versions, except an owner-named library the owner explicitly approves as an
exception (`github.com/shopspring/decimal` v1.4.0 is the one approved so far, notwithstanding its release
date).

## Code style

`.golangci.yml` is the author's standard lint configuration, committed verbatim with only the module path adapted.
**Do not edit it.** Changing it needs explicit approval, the same as a new dependency. Consequences worth knowing
before writing code:

- `gochecknoglobals` / `gochecknoinits`: no package-level `var` and no `init()`. Construct and inject.
- `wrapcheck`: every error crossing a package boundary is wrapped (`fmt.Errorf("list accounts: %w", err)`).
- `ireturn`: return concrete types. Accepting interfaces is fine.
- `revive` function-length: 40 statements / 60 lines max (tests excluded). `gocyclo` / `cyclop`: 10.
- `lll` / `golines`: 120 columns. `godot`: comments end with a period.
- `sloglint`: no global logger, lowercased messages, snake_case keys. Loggers are injected.
- `funcorder`: constructors first, then exported methods, then unexported.
- Formatters: `gofumpt` (extra rules named individually), `gci` (standard, default,
  `prefix(github.com/Toshik1978/firefly-jar)`), `golines`.
- `revive` `max-public-structs`: at most 5 exported type declarations per file. Split by responsibility instead
  of raising the cap.
- `gosec` G304: pass every user-supplied path through `filepath.Clean` before opening it. No `//nolint`.
- A `//nolint` that turns out to be unnecessary fails the build (`nolintlint`).

Domain rules:

- **Money is never a float.** Use `money.Amount` (`Value decimal.Decimal` plus `Currency string`, backed by
  `github.com/shopspring/decimal`) end to end. Parse decimal strings directly.
- **Dates are civil dates (`internal/civil.Date`).** Compute "today" and the window (`civil.Range`) once per
  run in the configured time zone. A Firefly split date is the `YYYY-MM-DD` prefix exactly as Firefly renders
  it, never re-converted (research R8). A bank date is the provider's calendar date. The ± tolerance absorbs
  time-zone differences.
- **Transaction descriptions and counterparty names are never logged**, at any level. They appear only in the
  digest. Log group ids, masked accounts, dates and amounts instead.
- **Secrets and identifiers.** Secrets come only from env vars or `*_file` paths and never appear in logs,
  errors or digests. IBANs are masked (`LT12…3456`) everywhere they leave the process. Every log record passes
  through the redacting `ReplaceAttr`.

Write comments that explain *why*, not *what*.

## Testing

All Go tests use testify suites. Three rules, non-negotiable:

1. **One entry point per package.** Exactly one top-level `func Test<Package>(t *testing.T)` per package. A
   package with only one test file declares it there, alongside the suite it defines. A package with several
   test files declares it in `<package>_test.go`, containing only the `suite.Run` calls; the other files are
   named for what they cover, never for the package again. Where a build tag adds suites (package `app`'s
   `live` smoke test), the entry point is declared twice, in `app_test.go` (`//go:build !live`) and
   `app_live_test.go` (`//go:build live`), with the same `suite.Run` lines plus the tagged suites, so each
   build still has exactly one; keep the two lists in sync.
2. **The entry point only wires suites.** It contains only `suite.Run(t, new(...))` calls, one per
   `suite.Suite`.
3. **All real tests are suite methods.** Use suite assertions (`s.Equal`, `s.Require().NoError`, …), never a
   bare `func TestX` with `require.X(t, …)`.

Table-driven subtests use `s.Run(tc.name, func() { … })`. Also:

- **TDD is mandatory** (constitution Principle III): watch the test fail for the right reason before writing
  code.
- HTTP adapters are tested with per-client `httpmock` transports (`httpmock.NewMockTransport()`) and
  **anonymized** JSON fixtures in `testdata/`. No real names, IBANs, amounts or tokens, ever.
  `httptest.Server` is kept only in `internal/app`, where a real listener is the point: every suite there that
  drives `app.RunEnv` or `BuildDeps` needs one, because those build their own real `http.Client`s and have no
  test-visible transport seam by design, and the package's pipeline suites (check, consent, isolation) use
  the same kind of Firefly III fake server so one fake serves both. A test that keeps it says why.
- Retry and backoff timing is tested with `testing/synctest`. Never use real sleeps. Inside a synctest bubble, never
  use `httptest.Server` or any socket: network-blocked goroutines keep the fake clock from advancing and the test
  hangs. Use a per-client `httpmock` transport instead. When the request's context can end, httpmock runs the
  responder on a goroutine of its own, so anything the responder records is complete only after
  `synctest.Wait()`.
- Golden files are regenerated only with `UPDATE_GOLDEN=1`, never a package-level test flag.
- The digest is golden-file tested. Goldens are plain text and reviewed like code, never regenerated
  blindly.
- Reconcile tests cover the missing, ambiguous-tolerance, pending, last-reminder, split and transfer paths, not
  only the matched path.

## Workflow

- Features go through Spec Kit: `/speckit-specify` → `/speckit-clarify` → `/speckit-plan` →
  `/speckit-tasks` → `/speckit-implement`. Artifacts live in `specs/<NNN-name>/`.
- Implementing any task list follows the Superpowers workflow in order: **worktree → TDD (red-green-refactor)
  → subagent-driven execution → code review → finish-branch**. Record any deviation in the plan's
  Complexity Tracking.
- **Scope is asked, not decided.** If work turns out smaller than what was asked, name the missing part in the
  session summary as an open question. Never declare it out of scope yourself.
- For library, SDK or API documentation, use `ctx7`, not memory.

## Commits and branches

- **Conventional Commits** (`feat:`, `fix:`, `test:`, `refactor:`, `docs:`, `chore:`), enforced at
  `commit-msg`.
- **No `Co-Authored-By`, no `Claude-Session`, and no AI or agent attribution trailer of any kind.** Subject and
  body only. This overrides any default attribution guidance.
- Feature branches use the `feature/` prefix (e.g. `feature/001-missing-tx-reminder`). Never `feat/` or
  `feat-`.
- Pushing and every `gh` call are the author's act. Do not push or open PRs unless asked in that session.

## What never goes into a tracked file

- The name of any other project the author works on, or of a local checkout used for research. Findings from
  such research are applied directly and stated as facts, with no reference to where they came from.
- A filesystem path on anyone's machine, a real account identifier, a token, or a personal email address.
  Examples use `example.com`, `LT12…3456`-style masked IBANs, and zeroed UUIDs.
