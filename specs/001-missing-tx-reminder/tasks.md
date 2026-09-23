---
description: "Task list for the Missing Transaction Reminder feature"
---

# Tasks: Missing Transaction Reminder

**Input**: Design documents from `/specs/001-missing-tx-reminder/`

**Prerequisites**: plan.md, spec.md, research.md (R1–R18), data-model.md, contracts/ (cli, config, digest, state),
quickstart.md, `.claude/CLAUDE.md` (repository rules)

**Tests**: REQUIRED. Constitution Principle III mandates TDD. In every pair below, the test task comes first
and must be **seen failing for the expected reason** before its implementation task starts.

**Organization**: Tasks are grouped by user story (spec.md US1–US4) so each story can be implemented and
tested as its own increment.

**Package renames after this list was built**: the task lines below are the historical record and keep the
package names they were built with. `internal/domain` has since split into `internal/money` (`Amount`,
`ParseAmount`) and `civil.Range` in `internal/civil` (formerly `domain.Window`), `internal/mapping` is now
`internal/accountmap`, and `internal/httpx` is now `internal/httpclient`.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: The user story the task belongs to (US1–US4)

## Conventions every task follows (from `.claude/CLAUDE.md`)

- **Test layout (testify suites)**:
  - Each package has exactly one entry file containing only `func Test<Pkg>(t *testing.T)` with one
    `suite.Run(t, new(XxxSuite))` per suite. The entry file is named in the package's first test task:
    `<pkg>_test.go`, or `<pkg>_pkg_test.go` when `<pkg>_test.go` already holds a suite.
  - A test task that adds a suite also registers it in that entry file, creating the file if it is missing.
  - All assertions live in suite methods (`s.Equal`, `s.Require().NoError`), and table cases use
    `s.Run(tc.name, …)`.
- **Timing and fixtures**: retry/backoff/sleep tests use `testing/synctest` and never real sleeps. **Inside a
  synctest bubble, never use `httptest.Server` or any loopback socket.** Goroutines blocked on network I/O keep
  the bubble from going idle, so the fake clock never advances and the test hangs. Use an in-process fake
  `http.RoundTripper`, or `net.Pipe` wired through `http.Transport.DialContext`. HTTP tests without fake time
  use `httptest.Server` with anonymized fixtures under `testdata/`: masked-style IBANs like
  `LT000000000000000001`, `example.com`, zeroed UUIDs, and no real names.
- **Do not edit `.golangci.yml`.** It is the author's standard config, committed verbatim. Consequences for task
  layouts:
  - at most 5 exported type declarations per file (revive `max-public-structs`); split files by
    responsibility;
  - every user-supplied file path goes through `filepath.Clean` before `os.Open`, `os.ReadFile` or
    `os.OpenFile` (gosec G304);
  - no package-level `var` (`gochecknoglobals`), including test flags.
- **Gate**: `task check` must pass after every task. No `//nolint` without a real finding.
- **Errors, logging, globals**: wrap errors at package boundaries (`fmt.Errorf("…: %w", err)`). No
  package-level `var` or `init()`. Loggers and clocks are injected, and "now" comes from an injected
  `func() time.Time`.
- **Commits**: Conventional Commits with **no AI or co-author trailers**. Branch
  `feature/001-missing-tx-reminder`.

## Preconditions (before T001)

Constitution §III step 1 needs a git repository. Before any task:

1. `git init`, then commit the existing design artifacts (`.specify/`, `.claude/`, `specs/`, `.golangci.yml`,
   `go.mod`) on `main` with a Conventional Commit such as `docs: add spec kit design for missing tx
   reminder`.
2. Create the worktree and branch with `superpowers:using-git-worktrees`, on branch
   `feature/001-missing-tx-reminder`.
3. Run every task below inside that worktree.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: A buildable, lint-clean, empty module with the full gate wired up.

- [x] T001 Create `Taskfile.yml` (go-task v3) with `vars: BINARY: firefly-jar`, `env: CGO_ENABLED: "0"` and
  these tasks:
  - `setup`: `mise install`; `go mod download`.
  - `format`: `golangci-lint fmt`.
  - `format:check`: `golangci-lint fmt --diff`.
  - `lint`: `golangci-lint run`.
  - `test`: `go test -race ./...`. Note: `-race` needs cgo, so this task sets `CGO_ENABLED: "1"` locally.
  - `build`: `go build -trimpath -ldflags "-s -w" -o {{.BINARY}} ./cmd/firefly-jar`.
  - `check`: `format:check` → `lint` → `test`.
  - `audit`: `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`.
  - `clean`: `rm -rf {{.BINARY}} dist/ cover.out`.
- [x] T002 [P] Create `.mise.toml` with `[tools]` `go = "1.27"`, `golangci-lint = "latest"` and `git-cliff = "2"`.
  Add a comment explaining why golangci-lint floats: it must match CI's `latest`. (git-cliff was later removed
  from the toolchain; this entry stays as the historical record of what T002 originally did.)
- [x] T003 [P] Create `.pre-commit-config.yaml`:
  - `default_install_hook_types: [pre-commit, pre-push, commit-msg]`.
  - Repo `https://github.com/compilerla/conventional-pre-commit` rev `v4.4.0`, hook `conventional-pre-commit`
    at stage `commit-msg`.
  - Local hook `golangci-lint fmt --diff` (`language: system`, `types: [go]`, `pass_filenames: false`,
    stage `pre-commit`).
  - Local hook `task check` (`pass_filenames: false`, stage `pre-push`).
- [x] T004 [P] Create `.github/workflows/ci.yml`, running on push/PR to `main` on `ubuntu-latest`:
  - Steps: checkout@v5, `jdx/mise-action@v3`, `go-task/setup-task@v2` (go-task is not provided by mise),
    `golangci/golangci-lint-action@v8` with `version: latest`, `task test`, and govulncheck.
  - Pin every action to a major tag, never an exact version.
  - Also create `.github/workflows/commit-lint.yml`, which runs conventional-commit checks on PR commits and
    **has no path filter**.
- [x] T005 [P] Create `.gitignore` covering `/firefly-jar`, `/dist/`, `cover.out`, `/config.yaml`, `*.pem`,
  `*.token`, `state.json`, `*.log`, `.env`, `.claude/worktrees/` (agent worktrees live inside the repo).
- [x] T006 Create the `cmd/firefly-jar/main.go` stub. `main()` calls `os.Exit(app.Run(os.Args[1:], os.Stdin,
  os.Stdout, os.Stderr))`. Create `internal/app/cli.go` with a `Run` that returns 2 and prints `usage:
  firefly-jar <check|auth|accounts> [flags]` to stderr. Keep `go.mod` at `go 1.27.1`, then confirm
  `task check` exits 0. Dependencies are added by the first task that imports them.

**Checkpoint**: `task check` is green on an empty module, and `pre-commit install` works.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The value types, config, logging, HTTP retry, state and bank abstractions every story needs.

**⚠️ CRITICAL**: No user-story task starts before this phase is complete.

### domain: exact money and civil dates (data-model "Value types", R15)

- [x] T007 [P] Write `AmountSuite` in `internal/domain/amount_test.go`, registered in
  `internal/domain/domain_test.go` (`TestDomain`). Cases:
  - `ParseAmount("12.34","EUR")` gives Minor 1234, Scale 2.
  - `"-12.40"` parses and is negative.
  - `"12.340000000000"` equals `"12.34"`, because normalization strips trailing fractional zeros.
  - `"0"` and `"0.00"` both report `IsZero`.
  - Rejected: `""`, `"1,23"`, `"1.2.3"`, `"abc"`, `" 1"`, and a value with more than 18 significant digits.
  - `Equal` is false when the currency differs.
  - `Neg` and `Abs`.
  - `Key()` is identical for equal values and currency, for grouping.
  - `Format(2)` gives `"-4.50"`, and `Format(0)` of 12.00 gives `"12"`.
- [x] T008 [P] Write `DateSuite` and `WindowSuite` in `internal/domain/date_test.go`:
  - `ParseDate("2026-09-22")`; `DateOf(t time.Time, loc)`, using the calendar date in `loc`.
  - `AddDays` across month, year and leap day; `DaysBetween` (signed); `Compare`/`Before`;
    `String()` gives `YYYY-MM-DD`.
  - `NewWindow(today, 30)` has `From = today − 29` and `To = today`; `Contains` includes both ends;
    `IsFirstDay(d)` is true only for `From`.
- [x] T009 Implement `internal/domain/amount.go`:
  - `type Amount struct{ Minor int64; Scale uint8; Currency string }`, parsed without float and normalized.
  - Methods: `Equal`, `IsZero`, `Neg`, `Abs`, `Sign`, `Key`, `Format(decimalPlaces uint8) string`.
  - Make T007 pass.
- [x] T010 Implement `internal/domain/date.go` (`Date{Year int; Month time.Month; Day int}`, `ParseDate`, `DateOf`,
  `AddDays`, `DaysBetween`, `Compare`, `String`) and `internal/domain/window.go` (`Window{From, To Date}`,
  `NewWindow`, `Contains`, `IsFirstDay`). Make T008 pass.

The owner widened the dependency policy on 2026-09-22 (see the epic's ruling comments). Three tasks follow,
added after T010 and run before T018:
- [x] T010a Replace the hand-rolled `Amount{Minor int64, Scale uint8, Currency string}` from T009 with
  `Amount{Value decimal.Decimal, Currency string}`, backed by `github.com/shopspring/decimal` v1.4.0
  (owner-approved). `ParseAmount` keeps its own grammar check ahead of `decimal.NewFromString` (stricter: no
  scientific notation, no thousands separators, more than 18 significant digits still rejected). Add a new
  `Add` method that sums Firefly split amounts and errors on currency mismatch (FR-009).
  `Equal`/`IsZero`/`Neg`/`Abs`/`Sign`/`Key`/`Format(decimalPlaces uint8) string` keep their T009 behavior. No
  float anywhere.
- [x] T010b Remove `internal/domain/date.go` (`Date`, `ParseDate`, `DateOf`, `AddDays`, `DaysBetween`,
  `Compare`, `String` from T008/T010) and add `internal/civil`, a trimmed copy of
  `cloud.google.com/go/civil` v0.123.0's `Date` (Apache-2.0 header kept; `Time`/`DateTime`, `database/sql`
  integration and `AddMonths`/`AddYears`/`Weekday` removed as unused). `DaysBetween` is replaced by
  `DaysSince` (the signed day count between two dates, the inverse of `AddDays`). `internal/domain/window.go`
  moves `Window{From, To civil.Date}` and its methods onto `civil.Date`, unchanged in behavior. Wherever a
  task in this file says `domain.Date` or `DaysBetween`, read `civil.Date` (`internal/civil`) and
  `DaysSince`.
- [x] T010c Update `plan.md`, `research.md`, `data-model.md`, `tasks.md` and `.claude/CLAUDE.md` to state the
  T010a/T010b dependency decisions, and the CLI decision (cobra, for T053/T054), as facts.

### redact: masking and secret scrubbing (FR-035, FR-036)

- [x] T011 [P] Write `RedactorSuite` in `internal/redact/redact_test.go`, registered in
  `internal/redact/redact_pkg_test.go` (`TestRedact`). Cases:
  - `MaskIBAN("LT121000011101001000")` gives `"LT12…1000"`; strings of 8 characters or fewer come back as
    `"****"`.
  - `MaskHash(h)` gives `"hash:" + first4 + "…" + last4`.
  - `New(secrets ...string).Scrub(s)` replaces every non-empty secret with `[REDACTED]`.
  - `Scrub` masks IBAN-shaped substrings (`[A-Z]{2}\d{2}[A-Z0-9]{11,30}`, case-insensitive) in free text,
    and IBANs in print form (4-character groups separated by single spaces, e.g. `LT12 3456 7890 1234 5678`)
    the same way; a run of words or a short number sequence is left alone.
  - A Telegram URL `https://api.telegram.org/bot<token>/sendMessage` has the token scrubbed even when it
    isn't in the secret list (`/bot[^/]+/` pattern).
- [x] T012 Implement `internal/redact/redact.go`: `type Redactor struct` with regexes compiled in `New`, no
  globals; `MaskIBAN` and `MaskHash` as pure functions. Make T011 pass.

### config: strict YAML, secrets, validation (contracts/config.md, FR-030, FR-034)

- [x] T013 [P] Add anonymized fixtures:
  - `testdata/config/valid.yaml`: every section from `contracts/config.md`, with `*_file` paths pointing
    to files under `testdata/config/secrets/` (mode 0600).
  - `testdata/config/minimal.yaml`: required fields only.
  - A test RSA key: `testdata/config/secrets/enablebanking.pem` (PKCS#8) and
    `testdata/config/secrets/enablebanking_pkcs1.pem`, both generated for tests only.
- [x] T014 [P] Write `LoadSuite` and `ValidateSuite` in `internal/config/config_test.go`, registered in
  `internal/config/config_pkg_test.go` (`TestConfig`).
  - **Loading and defaults:**
    - `valid.yaml` loads.
    - Defaults are `window_days 30`, `date_tolerance_days 3`, `consent_warn_days 7`, `log_level info`,
      `psu_type personal`.
    - An unknown key fails, and the error names the key and line.
  - **Range and format checks:**
    - `window_days` must be 1..365, `date_tolerance_days` 0..14, `consent_warn_days` 0..90.
    - An invalid `timezone` fails.
    - `firefly.url`: `https` passes; `http` passes only for `localhost`, loopback or private-LAN IPs.
      Suffixes `…/api` and `…/api/v1` normalize to base `…/api/v1`.
    - `email.port` must be 587 or 465.
  - **Override rules:** exactly one of `hash`/`iban`; exactly one of `firefly_account_id`/`exclude: true`;
    `bank` must be a key in `banks`. `currency` is optional.
  - **Secrets:**
    - The env var wins over the file: `FIREFLY_JAR_FIREFLY_TOKEN`, `FIREFLY_JAR_ENABLEBANKING_PRIVATE_KEY`
      (PEM text), `FIREFLY_JAR_TELEGRAM_TOKEN`, `FIREFLY_JAR_SMTP_PASSWORD`.
    - File contents are whitespace-trimmed.
    - A missing or unreadable secret is an error.
    - A secret file with group/other permission bits returns a **warning**, not an error.
    - The private key parses from both PKCS#1 and PKCS#8 PEM.
  - **Per-command validation and secrets**, per the table in contracts/config.md:
    - `ValidateFor(cmd Command, stdout bool) (Secrets, []string, error)` with `Command` =
      `CmdAuth|CmdAccounts|CmdCheck`. It resolves **only** the secrets that command needs and never reads the
      others.
    - Cases:
      - `auth` succeeds with the Firefly token, Telegram token and SMTP password files all missing;
      - `accounts` succeeds with notifier secrets missing but fails without the Firefly token;
      - `check --stdout` does not require notifier secrets or recipients;
      - `check` requires every configured notifier's secret and at least one recipient.
    - `log_file` directory existence and writability is checked for `check` only.
- [x] T015 Implement `internal/config`:
  - Types matching contracts/config.md. Keep at most 5 exported types per file, as revive
    `max-public-structs` in `.golangci.yml` requires. Use `config.go` (`Config`, `Bank`, `AccountRule`),
    `services.go` (`Firefly`, `EnableBanking`), `notify.go` (`Notify`, `Telegram`, `Email`) and `secrets.go`
    (`Secrets` plus resolution). Exported names, so later tasks can rely on them: `Config` (root), `Firefly{URL, TokenFile}`, `EnableBanking{AppID, PrivateKeyFile,
    RedirectURL, PSUType}`, `Bank{Name, Country, Display}` (in `Config.Banks map[string]Bank`),
    `AccountRule{Bank, Hash, IBAN, Currency string; FireflyAccountID string; Exclude bool}`,
    `Notify{Telegram *Telegram; Email *Email}`, `Telegram{BotTokenFile string; ChatIDs []int64}` and
    `Email{Host string; Port int; Username, PasswordFile, From string; To []string}`. Resolved secrets live
    in `Secrets{FireflyToken, TelegramToken, SMTPPassword string; PrivateKey *rsa.PrivateKey}`, never in the
    YAML structs.
  - `validate.go`: structural validation in `Load`, plus `ValidateFor(cmd, stdout)` for per-command secret
    resolution (T014).
  - `load.go`: `Load(path string, env func(string) string) (*Config, error)` using
    `github.com/goccy/go-yaml` with `yaml.Strict()`. Run `go get github.com/goccy/go-yaml@latest`.
  - Config path resolution: `--config` flag, then `FIREFLY_JAR_CONFIG`, then `/etc/firefly-jar/config.yaml`.
  - Every config and secret file path goes through `filepath.Clean` before it is read (gosec G304). No
    `//nolint`.
  - Make T014 pass.

### logging: file JSON + stderr WARN+, redacted (FR-038, FR-039, R14)

- [x] T016 [P] Write `LoggingSuite` in `internal/logging/logging_test.go`, registered in
  `internal/logging/logging_pkg_test.go`, with `bytes.Buffer` writers:
  - An INFO record goes to the file writer only, as JSON.
  - A WARN record goes to both; stderr uses the text format.
  - A `log_level: warn` file handler drops INFO.
  - `ReplaceAttr`:
    - masks IBAN-shaped values in string attributes;
    - replaces the values of keys `token`, `password`, `private_key`, `authorization` with `[REDACTED]`;
    - truncates `session_id` to `first4…`.
- [x] T017 Implement `internal/logging/logging.go`:
  - `New(file io.Writer, stderr io.Writer, level slog.Level, r *redact.Redactor) *slog.Logger`, built with
    `slog.NewMultiHandler`.
  - `OpenFile(path string) (*os.File, error)`, opening `filepath.Clean(path)` with `O_APPEND|O_CREATE|O_WRONLY`
    and mode 0600.
  - Make T016 pass.

### httpx: retries and client defaults (R12)

- [x] T018 [P] (FR-031) Write `RetrySuite` in `internal/httpx/retry_test.go`, registered in `internal/httpx/httpx_test.go`.
  Use `testing/synctest` with an **in-process scripted fake `http.RoundTripper`** as `Base`: it returns a
  queued response or error per attempt and records attempts. No `httptest` or sockets. Cases:
  - A GET that gets 500, 500, then 200 succeeds after 2 retries with waits of about 1s and 2s (jitter source
    injected and fixed).
  - A network error is retried.
  - 429 with `Retry-After: 2` waits 2s.
  - `Retry-After` as an HTTP-date is honored.
  - `Retry-After: 120` (over the 60s cap) stops at once and returns `*httpx.RetryAfterTooLongError` carrying
    the status.
  - 400, 401, 403 and 404 are never retried.
  - POST is never retried.
  - Response bodies of failed attempts are drained and closed.
  - Context cancellation stops the waiting.
  - At most 3 retries (4 attempts), then the last response or error is returned.
- [x] T019 Implement `internal/httpx/retry.go`: `RetryTransport{Base http.RoundTripper; MaxRetries int;
  BaseDelay, MaxWait, AttemptTimeout time.Duration; Jitter func() float64}`, with defaults 3, 1s, 60s, 30s
  and 20%; every attempt runs under `internal/httpx/timeout.go`'s `TimeoutTransport` (a 30 s deadline per
  attempt, released when the body is closed). Implement `internal/httpx/client.go`:
  `NewClient(rt http.RoundTripper) *http.Client` with no client-wide timeout (it would span the retries and
  `Retry-After` waits) and `CheckRedirect` returning `http.ErrUseLastResponse`. Make T018 pass.

### state: session file (contracts/state.md, FR-019, FR-037, R11)

- [x] T020 [P] Add fixtures `testdata/state/valid.json` (two banks, anonymized) and
  `testdata/state/bad_version.json` (`"version": 2`).
- [x] T021 [P] Write `StateSuite` in `internal/state/state_test.go`, registered in `internal/state/state_pkg_test.go`:
  - `Load` of a missing path returns an empty `State{Version: 1}` without error.
  - `Load(valid.json)` round-trips.
  - An unknown `version` is an error.
  - A file with group/other permission bits returns a warning.
  - `Save` in `t.TempDir()`:
    - writes mode `0600`;
    - `Put(bankKey, session)` replaces only that bank;
    - a simulated failure (read-only dir) leaves the old file intact and no `.state.*.tmp` behind.
- [x] T022 Implement `internal/state/state.go`:
  - Types `State{Version int; Sessions map[string]Session}`,
    `Session{Provider, SessionID string; ValidUntil, AuthorizedAt time.Time; Accounts []Account}` and
    `Account{UID, Hash, IBAN, Currency, Name string}`, with JSON tags exactly as in contracts/state.md.
  - `Load(path) (*State, []string, error)` and `Save(path, *State) error`, with the path passed through
    `filepath.Clean` (gosec G304).
  - `Save` follows R11: `CreateTemp` in the same dir, `Chmod(0600)`, write, `Sync`, `Close`, `Rename`, then
    fsync the dir; the temp file is removed on any error.
  - Make T021 pass.

### bank: provider abstraction (Principle IV, data-model "Bank side")

- [x] T023 [P] Write `ErrorsSuite` in `internal/bank/bank_test.go`, registered in `internal/bank/bank_pkg_test.go`:
  - `errors.Is` works through wrapping for `ErrConsentExpired`, `ErrConsentRevoked`, `ErrRateLimited` and
    `ErrDataIncomplete`.
  - `*bank.Error{Kind, Detail}` unwraps to its kind.
- [x] T024 (FR-022) Implement `internal/bank`, split to keep at most 5 exported types per file (revive
  `max-public-structs`): `bank.go` (`Account`, `Status`, `Transaction`), `provider.go` (`Provider`,
  `Authorizer`, `Pending`) and `errors.go` (sentinels, `Error`):
  - `Account{BankKey, UID, Hash, IBAN, Currency, Name string}`.
  - `Status` as an int enum `Booked|Pending|Void` with `String()`.
  - `Transaction{Account Account; Date domain.Date; Amount domain.Amount; Status Status; EntryRef,
    Description string}`.
  - `type Provider interface { Transactions(ctx context.Context, sessionID string, acc Account,
    from domain.Date) ([]Transaction, error) }`.
  - The provider-neutral consent flow (constitution §IV, data-model "Authorizer"): `type Authorizer interface
    { Begin(ctx, b config.Bank) (Pending, error); Complete(ctx, b config.Bank, p Pending, pastedRedirect
    string) (state.Session, error); Revoke(ctx, sessionID string) error }`, with `Pending{URL, State string}`.
  - Sentinel errors, `ErrStateMismatch`, and `Error`.
  - Make T023 pass.

- [x] T025 [P] Create `internal/firefly/types.go` with the domain types only, and no client code:
  - `Account{ID, Name, IBAN, Currency, Role string; DecimalPlaces uint8; Active bool}`.
  - `Entry{GroupID, AccountID string; Date domain.Date; Amount domain.Amount; Description string}`.
  - Add a package doc comment stating the read-only rule (FR-001).
  - This is type declarations with no behavior, so there is no test task. `task check` must stay green. It
    lets `mapping` and `reconcile` start in parallel with the Firefly client.

**Checkpoint**: The foundation packages are green. User stories can start.

---

## Phase 3: User Story 1: Get reminded about transactions I forgot to enter (Priority: P1) 🎯 MVP

**Goal**: `firefly-jar check` fetches bank transactions (from an existing session in the state file) and
Firefly III entries, matches them (FR-005–FR-013), and sends a digest to Telegram and/or email only when
something is missing. Exit code 0 means clean, 1 means missing found.

**Independent Test**: Provision `state.json` by hand with one session and one mapped account, and leave one bank
transaction unentered. `check --stdout` lists exactly that transaction with exit 1. After it is entered, the
run prints nothing and exits 0. The end-to-end version is T056.

### Enable Banking: JWT and transactions (R2, R5, R6, R7)

- [x] T026 [P] [US1] Write `SignerSuite` in `internal/bank/enablebanking/jwt_test.go`, registered in
  `internal/bank/enablebanking/enablebanking_test.go`:
  - The token has three base64url parts.
  - The header is exactly `{"typ":"JWT","alg":"RS256","kid":"<app_id>"}`.
  - Claims are `iss:"enablebanking.com"`, `aud:"api.enablebanking.com"`, and `exp = iat + 3600`.
  - The signature verifies with `rsa.VerifyPKCS1v15`, using the public half of the test key.
  - The cached token is reused until 5 minutes before `exp`, then re-signed (injected clock).
- [x] T027 [US1] Implement `internal/bank/enablebanking/jwt.go` (`Signer{appID; key *rsa.PrivateKey; now func()
  time.Time}` with `Token() (string, error)`), stdlib only. Make T026 pass.
- [x] T028 [P] [US1] Add anonymized fixtures under `testdata/enablebanking/`:
  - `tx_flat_p1.json`: `continuation_key: "k2"`. Include DBIT and CRDT; one `PDNG` and one `BOOK` sharing
    `entry_reference: "ER-1"`; `HOLD`, `OTHR`, `CNCL`, `RJCT`, `SCHD`; a zero amount; one entry with only
    `booking_date`; one with only `value_date`; one with no date at all
    (in `tx_nodate.json` separately); one missing `credit_debit_indicator` with amount `"-7.00"`.
  - `tx_flat_p2_empty.json`: an empty `transactions` array with `continuation_key: "k3"`.
  - `tx_flat_p3.json`: final page, `continuation_key: null`.
  - `tx_grouped.json`: `{"transactions":{"booked":[…],"pending":[…]}}`.
  - Error bodies `err_rate_limit.json`, `err_expired_session.json`, `err_revoked_session.json` and
    `err_closed_session.json`, shaped `{"message":…,"code":…,"error":…,"detail":…}`.
- [x] T029 [P] [US1] (FR-002, FR-005, FR-005a, FR-005b) Write `TransactionsSuite` in
  `internal/bank/enablebanking/transactions_test.go`:
  - **Request**: `GET /accounts/{uid}/transactions` has `date_from = from − 1 day` and **no** `date_to` or
    `transaction_status`. `Authorization: Bearer <jwt>` is present, and no header starts with `Psu-`.
  - **Pagination**: continuation keys are followed through the empty middle page. After 100 pages, the
    result is `ErrDataIncomplete`.
  - **Status mapping (R5)**: `BOOK` → Booked; `PDNG`, `HOLD`, `OTHR` → Pending; `CNCL`, `RJCT`, `SCHD` and any
    zero amount → Void.
  - **Date (FR-005a)**: `transaction_date` ?? `booking_date` ?? `value_date`; none of them →
    `ErrDataIncomplete`.
  - **Amount**: `abs(amount)` with sign `DBIT` → −, `CRDT` → +. A missing indicator uses the amount's own sign.
  - **Description**: `creditor.name` (DBIT) or `debtor.name` (CRDT), else the first `remittance_information`
    line, else `(no description)`. Control characters are removed.
  - **Other fields**: `EntryRef` = `entry_reference`, and `transaction_id` is ignored. The grouped response
    shape parses the same as the flat one.
  - **Errors**: 429 with `ASPSP_RATE_LIMIT_EXCEEDED` (after retries are exhausted) → `ErrRateLimited`.
    `EXPIRED_SESSION` → `ErrConsentExpired`. `REVOKED_SESSION`/`CLOSED_SESSION` → `ErrConsentRevoked`. Any
    other error → `*bank.Error` whose detail is the redacted `message`.
- [x] T030 [US1] Implement:
  - `internal/bank/enablebanking/models.go`: hand-written structs for the fields used only.
  - `client.go`: `Client{baseURL string; http *http.Client; signer *Signer; redactor *redact.Redactor}`,
    `New(...)`, base URL `https://api.enablebanking.com`, built on `httpx`.
  - `transactions.go`: `Transactions` satisfying `bank.Provider`.
  - `errors.go`.
  - Make T029 pass.

### Firefly III: read-only client (R8, FR-001, FR-006, FR-006a, FR-009)

- [x] T031 [P] [US1] Write `ReadOnlySuite` in `internal/firefly/readonly_test.go`, registered in
  `internal/firefly/firefly_test.go`:
  - POST, PUT, PATCH, DELETE, HEAD, OPTIONS, TRACE, CONNECT and a made-up extension method through
    `ReadOnlyTransport` return `ErrWriteForbidden`, and the `httptest.Server` hit counter stays **0**.
  - Only GET passes through.
  - A 302 redirect to a second `httptest.Server` is **not followed**: the second server gets 0 hits and the
    client returns an error.
  - The `Authorization: Bearer <token>` header is sent with the token trimmed of whitespace and newlines, and
    `Accept: application/json` is sent.
- [x] T032 [US1] Implement `internal/firefly/readonly.go` (`ReadOnlyTransport`, `ErrWriteForbidden`) and
  `internal/firefly/client.go`:
  - `New(baseURL, token string, base http.RoundTripper) *Client` composing
    `ReadOnlyTransport{httpx.RetryTransport{base}}`, using `httpx.NewClient` so redirects are never followed.
  - Header injection.
  - Make T031 pass.
- [x] T033 [P] [US1] Add anonymized fixtures under `testdata/firefly/`:
  - `accounts_p1.json` and `accounts_p2.json`: `total_pages: 2`. `p2` repeats `current_page: 1` to prove the
    client counts pages itself. Accounts include one with a lowercase IBAN, one with `active: false`, one
    `ccAsset` role, one with no `active` key, and two sharing an IBAN in EUR and USD.
  - `accounts_nometa.json`.
  - `acc_tx_p1.json` and `acc_tx_p2.json`: a split group `id "7"` whose splits arrive on both pages. Also:
    - a withdrawal and a deposit;
    - a transfer out and a transfer in;
    - a cross-currency transfer into the account where `currency_code` is not the account's currency but
      `foreign_currency_code` is;
    - an `opening balance` and a `reconciliation` split (must be ignored), plus a split type
      `liability credit`;
    - a split in a foreign currency with no matching `foreign_amount` (skipped);
    - 12-decimal amounts like `"12.340000000000"`;
    - `date: "2026-09-20T23:30:00+02:00"`.
  - Error bodies `err_401.json`, `err_404.json`, `err_422.json`.
- [x] T034 [P] [US1] Write `AccountsSuite` in `internal/firefly/accounts_test.go`:
  - `ListAccounts` requests `type=asset&limit=500&page=N`.
  - It stops at `total_pages` using its own counter, even though `current_page` is stuck.
  - Missing `meta` means a single page.
  - Mapping: IBAN is uppercased and `null` becomes `""`; `active` defaults to true when missing;
    `currency_code` and `currency_decimal_places` map; `account_role` maps to `Role`.
  - A 401 maps to `ErrUnauthorized` with detail "Firefly token rejected".
- [x] T035 [P] [US1] Write `TransactionsSuite` in `internal/firefly/transactions_test.go`:
  - `ListAccountTransactions(ctx, accountID, currency string, start, end domain.Date)` sends
    `start`/`end` as `YYYY-MM-DD`, plus `types=withdrawal,deposit,transfer`, `type=default` and `limit=500`.
  - Group "7" is merged across pages by group id and `transaction_journal_id` into **one** entry whose amount
    is the signed sum.
  - Only `withdrawal`, `deposit` and `transfer` splits count. An unknown type string does not fail decoding.
  - Amount choice: `amount` if `currency_code == account currency`; else `foreign_amount` if
    `foreign_currency_code` matches; else the split is skipped with a DEBUG log carrying only the group id,
    `account_id` (the Firefly III account id), date and amount. The description is never logged
    (constitution §V).
  - Sign: `source_id == account` → −, `destination_id == account` → +.
  - Entry date is the `YYYY-MM-DD` prefix of the first counted split's `date`, so the fixture gives `2026-09-20`.
  - After 200 pages the result is `ErrDataIncomplete`. A 404 maps to `ErrNotFound`. A non-JSON error body
    yields the status code only.
- [x] T036 [US1] Implement:
  - `internal/firefly/models.go` (hand-written structs; `type` as a string).
  - `internal/firefly/accounts.go` (`ListAccounts`, returning `[]Account` from `types.go`).
  - `internal/firefly/transactions.go` (`ListAccountTransactions`, returning `[]Entry` from `types.go`).
  - `internal/firefly/errors.go`.
  - The package exposes **no** method other than `ListAccounts` and `ListAccountTransactions`.
  - Make T034 and T035 pass.

### mapping: automatic IBAN+currency resolution (FR-014, FR-016)

- [x] T037 [P] [US1] Write `AutoSuite` in `internal/mapping/mapping_test.go`, registered in
  `internal/mapping/mapping_pkg_test.go`:
  - Exactly one active Firefly account with an equal normalized IBAN **and** equal currency → `Auto`.
  - Two such accounts → `Ambiguous`, with both in `Candidates`.
  - None → `Unmapped`.
  - An inactive account is never an auto candidate.
  - A bank account without an IBAN → `Unmapped`.
  - Spaces and letter case in the IBAN are ignored.
  - EUR and USD bank accounts sharing an IBAN each map to the Firefly account of their own currency
    (spec US3 scenario 2).
- [x] T038 [US1] Implement `internal/mapping/mapping.go`:
  - `Status` enum `Auto|Override|Excluded|Ambiguous|Unmapped`.
  - `Mapping{Bank bank.Account; Status Status; Firefly *firefly.Account; Candidates []firefly.Account;
    Detail string}`.
  - `Resolve(banks []bank.Account, ff []firefly.Account, rules []config.AccountRule) []Mapping`, applying the
    resolution order from data-model.md. `rules` is accepted but only auto-mapping is implemented here;
    overrides come in T066.
  - Make T037 pass.

### reconcile: pure matching (FR-007–FR-011, FR-026, data-model "Algorithm")

- [x] T039 [P] [US1] (FR-005b, FR-007, FR-008, FR-010, FR-011, FR-025a, FR-026) Write `ReconcileSuite` in
  `internal/reconcile/reconcile_test.go`, registered in
  `internal/reconcile/reconcile_pkg_test.go`, as table-driven cases. **Every case asserts the invariant**
  `len(inWindow) == len(Matched)+len(Missing)+Deduplicated+Void`. Cases:
  - Spec US1 scenario 1: −12.40 EUR on 09-20 vs Firefly −12.40 on 09-21, tolerance 3 → matched.
  - Scenario 2 → missing.
  - Scenario 3: two −5.00 on the same day, one Firefly entry → exactly one missing.
  - Scenario 4: pending unmatched → `Missing.Pending`.
  - Scenario 5: a transaction on `window.From` → `LastReminder`.
  - Tolerance edges: a difference of exactly 3 days matches; 4 does not.
  - Greedy order: bank txs on 09-10 and 09-12 with Firefly entries on 09-11 and 09-13 → both match.
  - A different currency never matches. A different sign never matches.
  - Bank transactions outside the window are excluded from every count.
  - `Void` is counted and never missing.
  - Deduplication: Pending and Booked with the same `EntryRef`, both in the window → Deduplicated 1, and the
    Booked one is reconciled. If the Booked twin is outside the window, the Pending one stays.
  - An empty `EntryRef` never deduplicates.
  - FR-010: Firefly entries with no bank counterpart produce no Missing item and no count change.
  - Hints (FR-025a, data-model step 6):
    - Bank −4.50 on 09-10 and 09-15, one Firefly −4.50 on 09-13, tolerance 3 → 09-10 is matched. 09-15 is
      missing with `Hint{Kind: Taken, Date: 09-13}`.
    - Bank −9.99 on 09-10, Firefly −9.99 on 09-15, tolerance 3 → missing with `Hint{Kind: NearMiss,
      Date: 09-15}` (5 ≤ 2×3).
    - At 7 days apart there is no hint (7 > 6).
    - A different amount or currency never produces a hint.
    - With two candidates, the nearest wins, then the earlier date, then the lower group id.
    - A Taken candidate beats a NearMiss one at equal distance.
    - Hints never change the matched and missing counts.
  - Determinism: shuffled input orders give an identical result, including hints.
- [x] T040 [US1] Implement `internal/reconcile/reconcile.go`:
  - `Result{Matched []Pair; Missing []Missing; Deduplicated, Void int}`, with
    `Pair{Bank bank.Transaction; Firefly firefly.Entry}` and
    `Missing{Tx bank.Transaction; Pending, LastReminder bool; Hint *Hint}`, with
    `Hint{GroupID string; Date domain.Date; Kind HintKind}` (`Taken|NearMiss`).
  - `Reconcile(txs []bank.Transaction, entries []firefly.Entry, tolerance int, w domain.Window) Result`,
    following data-model steps 1–6 with a stable sort.
  - Make T039 pass.

### report: run outcome rules (FR-024, FR-029, data-model "Run outcome")

- [x] T041 [P] [US1] Write `ReportSuite` in `internal/report/report_test.go`, registered in
  `internal/report/report_pkg_test.go`:
  - `ExitCode()`:
    - 0 when clean;
    - 0 with consent warnings only;
    - 1 with missing and everything checked;
    - 2 when any account is unchecked, any problem exists, or `Delivery.Attempted > 0 && Succeeded == 0`;
    - 2 takes precedence over 1.
  - `DigestNeeded()` is true only when there is a missing transaction, a consent warning, an unchecked
    account or a problem.
  - `Summary()` counts: accounts checked/unchecked, matched, missing, deduplicated, void.
- [x] T042 [US1] Implement `internal/report`, split to keep at most 5 exported types per file (revive
  `max-public-structs`): `report.go` (`RunReport`, `AccountResult`, `Problem`), `unchecked.go` (`Unchecked`,
  `UncheckedCode`) and `delivery.go` (`Delivery`, `DeliveryFailure`, `ConsentWarning`):
  - `AccountResult{Mapping mapping.Mapping; Result reconcile.Result; Unchecked *Unchecked}`.
  - `Unchecked{Code UncheckedCode; Detail string}`, where the codes are the data-model list:
    `ConsentExpired, ConsentRevoked, RateLimited, BankError, BankDataIncomplete, FireflyError,
    FireflyDataIncomplete, Unmapped, Ambiguous, OverrideTargetMissing`, each with a human text such as
    "rate limited" or "ambiguous mapping". "Not authorized" is a run-level `Problem`, never an unchecked
    code, because a bank without a session has no known accounts.
  - `ConsentWarning{BankKey string; ValidUntil time.Time; DaysLeft int}`.
  - `Problem{Scope, Reason string}`.
  - `Delivery{Attempted, Succeeded int; Failures []DeliveryFailure}`.
  - `RunReport{Window domain.Window; Accounts []AccountResult; ConsentWarnings []ConsentWarning;
    Problems []Problem; Delivery Delivery}`.
  - Make T041 pass.

### digest: text rendering (contracts/digest.md, FR-025)

- [x] T043 [P] [US1] Write `RenderSuite` in `internal/digest/digest_test.go`, registered in
  `internal/digest/digest_pkg_test.go`, with golden files `testdata/digest/full.golden`,
  `missing_only.golden` and `consent_only.golden`. Regenerate them only when `UPDATE_GOLDEN=1` is set, read with
  `os.Getenv` inside the suite. No `flag` package variable (`gochecknoglobals`). Cases:
  - Header: `firefly-jar: N missing, M unchecked account(s) (window FROM – TO)`.
  - Sections in order, `⚠ Problems`, `⏰ Consent`, `Missing in Firefly III`, with empty sections omitted.
  - Account heading: `<bank display> · <masked IBAN or hash:xxxx…xxxx> · <name> (<CUR>)`.
  - Line: `- YYYY-MM-DD  -4.50 EUR  DESCRIPTION  ⏳ pending  🔚 last reminder`.
  - Sorting:
    - lines by date descending, then amount, then description;
    - accounts by bank key, then masked identifier, then currency.
  - Descriptions truncated to 60 characters with `…`, and control characters removed.
  - Consent line: `- <bank>: consent expires YYYY-MM-DD (N days) — run: firefly-jar auth <bank>`.
  - An ambiguous unchecked line lists `(Firefly #21, #22)`.
  - Hints (contracts/digest.md): `≈ Firefly #7 on 2026-09-13 (paired with another)` for Taken, and
    `≈ Firefly #9 on 2026-09-15 (5 days apart)` for NearMiss, appended after the flags. `full.golden` contains
    both.
  - No output contains an unmasked IBAN.
  - `Subject` equals the header line.
- [x] T044 [US1] Implement `internal/digest/digest.go` (`Digest{Subject string; Lines []string}`,
  `Render(r report.RunReport, banks map[string]config.Bank) Digest`, `Text() string`). Make T043 pass.

### notify: fan-out, Telegram, email (FR-023, FR-027, FR-028, R9, R10)

- [x] T045 [P] [US1] Write `FanOutSuite` in `internal/notify/notify_test.go`, registered in
  `internal/notify/notify_pkg_test.go`:
  - Every notifier is called even when an earlier one fails.
  - Results are merged into `report.Delivery` (Attempted = total recipients, Succeeded, and Failures with
    masked recipient and redacted reason).
- [x] T046 [US1] Implement `internal/notify/notify.go` (`type Notifier interface { Name() string; Send(ctx,
  digest.Digest) []Result }`, `Result{Recipient string; Err error}`, `FanOut(ctx, []Notifier, digest.Digest)
  report.Delivery`). Make T045 pass.
- [x] T047 [P] [US1] Write `TelegramSuite` in `internal/notify/telegram/telegram_test.go`, registered in
  `internal/notify/telegram/telegram_pkg_test.go`. The request-shape, splitting and secrecy cases use
  `httptest`. The pacing and retry cases run inside `synctest` with an **in-process fake `http.RoundTripper`**
  injected through the notifier's `*http.Client`, with no sockets.
  - **Request**: `POST /bot<token>/sendMessage` with a JSON body `{chat_id, text,
    link_preview_options:{is_disabled:true}}` and **no** `parse_mode`.
  - **Splitting**:
    - A digest over 4000 UTF-16 code units (emoji count as 2) splits only at line boundaries.
    - Continuation parts repeat the header with a ` (2/3)` suffix.
    - Joining the parts back (without the repeated headers) gives exactly the original lines.
  - **Pacing and retries**:
    - Parts to one chat are sent sequentially, at least 1.1s apart.
    - A 429 with `parameters.retry_after: 3` waits 3s and retries.
    - A 5xx is retried.
    - 400, 401 and 403 fail that recipient at once.
  - **Recipients**: each chat id is attempted independently.
  - **Secrecy**: no returned error or log line contains the bot token.
- [x] T048 [US1] Implement `internal/notify/telegram/telegram.go` (`New(token string, chatIDs []int64, hc
  *http.Client, now/sleep injected) *Notifier`), with base URL `https://api.telegram.org` overridable for
  tests. Make T047 pass.
- [x] T049 [P] [US1] Write `EmailSuite` in `internal/notify/email/email_test.go`, registered in
  `internal/notify/email/email_pkg_test.go`, against an in-test fake SMTP server (`net.Listen` on
  127.0.0.1). The server speaks EHLO, STARTTLS with a test certificate injected via `RootCAs`, AUTH PLAIN,
  MAIL, RCPT and DATA, and can also run implicit TLS.
  - **Port 587**: STARTTLS is required, and a server without it fails the send before AUTH.
  - **Port 465**: implicit TLS works.
  - **Message**:
    - headers `From`/`To` built with `mail.Address`, `Subject` Q-encoded UTF-8, `Date`, a random
      `Message-ID`, `MIME-Version: 1.0`, `Content-Type: text/plain; charset=utf-8`,
      `Content-Transfer-Encoding: quoted-printable`;
    - CRLF line endings;
    - a header value containing CR or LF is rejected.
  - **Recipients**: one `to` rejected at RCPT → that recipient fails and the others are delivered.
  - **Transport**: the connection deadline is applied.
- [x] T050 [US1] Implement `internal/notify/email/email.go` (`New(cfg config.Email, password string, tlsCfg
  *tls.Config) *Notifier`, using `net/smtp` with explicit STARTTLS or implicit TLS per R10, never
  `smtp.SendMail`). Make T049 pass.

### app: the check pipeline and CLI (FR-003, FR-004, FR-012, FR-013, FR-039, contracts/cli.md)

- [x] T051 [P] [US1] Write `CheckSuite` in `internal/app/check_test.go`, registered in `internal/app/app_test.go`.
  Use a fake `bank.Provider`, an `httptest` Firefly serving `testdata/firefly/*`, a recording fake
  `notify.Notifier`, a fixed clock (`2026-09-22 10:00 Europe/Vilnius`) and `bytes.Buffer` log writers. Cases:
  - Spec US1 scenarios 1, 2, 3, 6 and 7 end to end.
  - A clean run gives exit 0, **zero** notifier calls, and empty stdout and stderr.
  - A missing-found run gives exit 1, one digest delivered, and empty stdout and stderr.
  - `--stdout` prints the digest to stdout with no notifier calls and the same exit code.
  - The window is computed once from the injected clock.
  - Firefly is queried from `window.From − tolerance` to `today + tolerance`.
  - The bank is queried `from = window.From`.
  - One INFO summary record is written to the file log with `window`, `accounts_checked`,
    `accounts_unchecked`, `accounts_excluded` (added by fj-xwu.13), `matched`, `missing`, `deduplicated` and
    `void`.
- [x] T052 [US1] Implement `internal/app/check.go`:
  - `type Deps struct{ Config *config.Config; State *state.State; Provider bank.Provider; Firefly
    *firefly.Client; Notifiers []notify.Notifier; Log *slog.Logger; Now func() time.Time; Stdout io.Writer }`.
  - `Check(ctx, deps, opts CheckOptions) (report.RunReport, int)`, in this order:
    1. List Firefly accounts **first**, so an unreachable Firefly never costs bank quota.
    2. For each configured bank with a session (sequentially), for each account: map it; if mapped, fetch
       bank transactions and Firefly entries, then reconcile.
    3. Build the report.
    4. Render the digest if needed, then send it or print it.
    5. Log the summary.
  - Make T051 pass.
- [x] T053 [P] [US1] Write `CLISuite` in `internal/app/cli_test.go`:
  - `Run([]string{"--version"}, …)` exits 0 and prints the version.
  - `Run([]string{"bogus"})` exits 2 with usage on stderr.
  - `check --help` exits 0.
  - `check` with `--config` pointing at a valid fixture and fakes injected through a `Factory` seam dispatches
    to `Check`.
  - Per-command wiring (contracts/config.md table):
    - `check --stdout`, with the Telegram token and SMTP password files missing, builds no notifier and exits
      0 or 1;
    - `check` without `--stdout` builds exactly the notifiers configured.
- [x] T054 [US1] Complete `internal/app/cli.go` and add `internal/app/wire.go`:
  - `Run` parses subcommands with `github.com/spf13/cobra` (owner-approved 2026-09-22), not
    `flag.NewFlagSet`: a cobra root command wired with `SetArgs`/`SetIn`/`SetOut`/`SetErr` and
    `SilenceUsage`/`SilenceErrors`, exit codes mapped explicitly, keeping `Run(args, stdin, stdout, stderr)
    int` as the entry point for `check [--stdout]`, `auth <bank>`, `accounts [--ids]`, and global
    `--config`, `--version`, `--help`.
  - `wire.go` builds deps **per command** from the `Secrets` returned by `config.ValidateFor(cmd, stdout)`:
    - Redactor: from the resolved secrets only. Unresolved secrets are never in memory, so they cannot leak.
    - Logger: file (JSON) plus stderr (WARN and above) for `check`; stderr only (WARN and above) for `auth`
      and `accounts`, which never open `log_file`.
    - State, and the Enable Banking client, for every command.
    - Firefly client for `accounts` and `check`.
    - Notifiers only for `check` without `--stdout`, and only the channels configured.
  - A 10-minute overall context applies.
  - `auth` and `accounts` print "not implemented" and exit 2 until US2 and US3.
  - Keep `cmd/firefly-jar/main.go` under 30 lines.
  - Make T053 pass.
- [x] T055 [P] [US1] Add the build-tagged live smoke test `internal/app/live_test.go` (`//go:build live`). It runs
  `check --stdout` against the config in `FIREFLY_JAR_CONFIG` and asserts exit ∈ {0,1}. It is excluded from
  `task check`.
- [x] T056 [US1] Write the US1 acceptance test `internal/app/acceptance_us1_test.go` (`AcceptanceUS1Suite`) covering
  the Independent Test: one unentered transaction gives exit 1 and a digest containing exactly it. The
  Firefly fixture is then updated to include it, and the next run gives exit 0 and sends nothing.

**Checkpoint**: The MVP works. `check` reminds the owner about missing transactions through Telegram and email.

---

## Phase 4: User Story 2: Connect a bank and keep the connection alive (Priority: P2)

**Goal**: `firefly-jar auth <bank>` creates or renews a session interactively (FR-018, FR-019). `check` warns
before consent expires and reports expired, revoked or missing sessions (FR-020, FR-021).

**Independent Test**: Run `auth` against a fake Enable Banking server with a pasted redirect. The state is
saved with mode 0600. A session expiring in 5 days produces a digest consent warning with exit unchanged. An
expired session makes its accounts unchecked with exit 2.

- [x] T057 [P] [US2] Add fixtures `testdata/enablebanking/aspsps.json` (includes `maximum_consent_validity:
  15552000`), `auth_start.json` and `session_created.json` (two accounts, one without `account_id.iban`), plus
  error `err_already_authorized.json`.
- [x] T058 [P] [US2] (FR-002, FR-018, FR-022) Write `AuthSuite` in `internal/bank/enablebanking/auth_test.go`:
  - `FindASPSP(ctx, name, country, psuType)` calls `GET /aspsps?country=LT&psu_type=personal&service=AIS` and
    matches the name exactly. A missing bank gives an error listing close names.
  - `StartAuth` body:
    - `access.valid_until = now + maximum_consent_validity − 1h` (RFC 3339);
    - `access.transactions = true` and `access.balances = false`;
    - `aspsp{name,country}`;
    - `state` is 32 lowercase hex characters from `crypto/rand`;
    - `redirect_url` and `psu_type` as configured.
  - `ParseRedirect(pasted, expectedState)`:
    - `error=access_denied&error_description=Cancelled by user` → an error containing the description;
    - a state mismatch → `ErrStateMismatch`;
    - a missing `code` → an error;
    - surrounding whitespace is tolerated.
  - `CreateSession(code)`:
    - `POST /sessions` returns session id, `access.valid_until`, and accounts with `uid`,
      `identification_hash`, `account_id.iban` (may be empty), `currency` and `name`;
    - POST is **not retried**: a server returning 500 is hit exactly once.
  - `DeleteSession(id)` sends `DELETE /sessions/{id}`.
- [x] T059 [US2] Implement `internal/bank/enablebanking/auth.go` (`FindASPSP`, `StartAuth`, `ParseRedirect`,
  `CreateSession`, `DeleteSession`). Add `Authorizer` methods on the client satisfying `bank.Authorizer`:
  - `Begin` = `FindASPSP` + `StartAuth`;
  - `Complete` = `ParseRedirect` + `CreateSession`, mapped to `state.Session{Provider: "enablebanking", …}`;
  - `Revoke` = `DeleteSession`.
  - Add a compile-time assertion `var _ bank.Authorizer = (*Client)(nil)` inside a function or test, since
    package-level vars are banned.
  - Make T058 pass.
- [x] T060 [P] [US2] Write `AuthCommandSuite` in `internal/app/auth_test.go`. It scripts stdin with the pasted URL
  and injects a **fake `bank.Authorizer`**, so `app` never imports `enablebanking` in tests.
  - **Success**:
    - The output matches contracts/cli.md: `Open this link and log in to <Name> (<CC>):`, the URL, a paste
      prompt, then `Connected <Name> (<CC>): 2 accounts, consent valid until YYYY-MM-DD.`
    - Exit 0, and the state file is mode 0600 containing `hash`, `iban`, `currency` and `name`.
    - Another bank's existing session is preserved.
    - The previous session of the same bank gets `DELETE` **after** the save, and a failing DELETE is only
      logged as WARN.
  - **Failures**:
    - A state mismatch or `error=` gives exit 2 and a byte-identical state file.
    - An unknown bank key gives exit 2 before any network call.
    - A provider error gives exit 2 with nothing saved.
- [x] T061 [US2] Implement `internal/app/auth.go` against `bank.Authorizer` only. `wire.go` selects the
  implementation from the provider name (`enablebanking` for now). Wire `auth <bank>` in
  `internal/app/cli.go`. Make T060 pass.
- [x] T062 [P] [US2] Write `ConsentSuite` in `internal/app/consent_test.go`, using the fixed clock and states. Each
  row of the data-model consent table is covered:
  - `now < ValidUntil − warn_days` → OK, no warning.
  - Expiring in 5 days with `warn_days` 7 → `ConsentWarning{DaysLeft: 5}`, accounts still checked, exit
    unchanged, and the digest has the `⏰ Consent` line.
  - `now ≥ ValidUntil` → all of that bank's accounts are unchecked `ConsentExpired`, the provider is **never
    called**, and exit is 2.
  - The provider returns `ErrConsentExpired` or `ErrConsentRevoked` mid-run → all remaining accounts of that
    bank are unchecked with the matching code, and other banks are still checked.
  - A configured bank with no session → `Problem` "not authorized — run: firefly-jar auth <bank>", exit 2.
  - A state session for a bank no longer in config → WARN, ignored.
- [x] T063 [US2] Implement `internal/app/consent.go` (`consentState(session, now, warnDays)`) and integrate it into
  `internal/app/check.go`. Make T062 pass along with every earlier `internal/app` suite.
- [x] T064 [US2] Write the US2 acceptance test `internal/app/acceptance_us2_test.go` covering spec US2 scenarios 1–5
  end to end.

**Checkpoint**: The bank connection lifecycle works, and consent problems are always visible.

---

## Phase 5: User Story 3: Trust that every account is actually covered (Priority: P3)

**Goal**: Configuration overrides (by `hash` or `iban` with optional `currency`, map or exclude), and the
read-only `accounts [--ids]` command (FR-015, FR-017).

**Independent Test**: Two bank accounts, one auto-mapped by IBAN and one without an IBAN. `accounts` shows
`auto` and `unmapped` with exit 2. After adding a `hash` override, both are mapped with exit 0.

- [x] T065 [P] [US3] Write `OverrideSuite` in `internal/mapping/override_test.go`:
  - A `hash` rule → `Override` to the given Firefly id.
  - An `iban` rule without `currency` matches all currencies, and one with `currency` only that currency.
  - An `exclude: true` rule → `Excluded`.
  - Overrides take precedence over auto-mapping.
  - The first matching rule wins.
  - A rule for another `bank` key never matches.
  - An override to a non-existent Firefly id → `Unmapped` with detail "override target #N not found"
    (`OverrideTargetMissing`).
  - An override may target an inactive account.
- [x] T066 [US3] Implement override resolution in `internal/mapping/mapping.go` (step 1 of the data-model
  resolution order). Make T065 pass along with `AutoSuite`.
- [x] T067 [P] [US3] Write `AccountsCommandSuite` in `internal/app/accounts_test.go`:
  - **Output** follows contracts/cli.md:
    - columns `BANK ACCOUNT NAME CUR STATUS FIREFLY`, with a masked IBAN or `hash:xxxx…xxxx`;
    - status is one of `auto|override|excluded|ambiguous|unmapped`;
    - the Firefly column shows `#12 Household EUR`, or `#21, #22` for ambiguous, or `—`.
  - `--ids` adds a trailing `HASH` column with the full hash.
  - Exit 0 when every non-excluded account is mapped, otherwise 2.
  - **Read-only**:
    - the state file's mtime and bytes are unchanged;
    - the Firefly fake records only GETs;
    - no bank transaction fetch happens, because accounts come from the state snapshot.
- [x] T068 [US3] Implement `internal/app/accounts.go` (a `text/tabwriter` table) and wire `accounts [--ids]` in
  `internal/app/cli.go`. Make T067 pass.
- [x] T069 [US3] Write the US3 acceptance test `internal/app/acceptance_us3_test.go` covering spec US3 scenarios 1–6:
  - multi-currency mapping;
  - ambiguous → unchecked "ambiguous mapping (Firefly #21, #22)" in the digest with exit 2;
  - excluded is not reported;
  - unmapped → unchecked "no Firefly III account mapped".

**Checkpoint**: Coverage is visible and fixable. No account is silently skipped.

---

## Phase 6: User Story 4: Know when the check itself failed (Priority: P4)

**Goal**: Per-account failure isolation, loud partial failures, delivery fallback, and fail-fast config
(FR-028–FR-033).

**Independent Test**: With Firefly unreachable, a problem-only digest is sent and the exit is 2. With
Telegram failing and email working, the email is delivered and stderr shows a WARN.

- [x] T070 [P] [US4] (FR-032, FR-033) Write `IsolationSuite` in `internal/app/isolation_test.go`:
  - Bank A's provider returns `ErrRateLimited` → A's accounts are unchecked "rate limited", bank B is still
    reconciled and its missing transactions reported, and exit is 2.
  - A generic `*bank.Error` → unchecked "bank error: <redacted detail>".
  - `ErrDataIncomplete` → unchecked "bank data incomplete".
  - Firefly `ListAccounts` fails after retries → a problem-only digest ("Firefly III unreachable: …"),
    **zero** bank provider calls, exit 2.
  - Firefly `ListAccountTransactions` fails for one account → only that account is unchecked
    `FireflyError`/`FireflyDataIncomplete`.
  - Every exit-2 run prints one ERROR summary line to stderr.
- [x] T071 [US4] Implement per-bank and per-account error isolation plus the stderr ERROR summary in
  `internal/app/check.go`. Make T070 pass.
- [x] T072 [P] [US4] Write `DeliverySuite` in `internal/app/delivery_test.go`:
  - Telegram fails and email succeeds → exit follows the findings (1), with one WARN on stderr naming the
    channel and the masked recipient.
  - Every recipient fails → the full digest text is written to stderr and exit is 2.
  - A partial failure within one channel (one chat id of two) → the other is delivered.
- [x] T073 [US4] Implement the delivery fallback in `internal/app/check.go`. Make T072 pass.
- [x] T074 [P] [US4] Write `FailFastSuite` in `internal/app/failfast_test.go`:
  - For `check`, `auth` and `accounts`, each of these gives exit 2 with a stderr message naming the key or
    file, and **zero** hits on the Firefly, Enable Banking, Telegram and SMTP fakes:
    - an unknown config key;
    - a bad or missing Enable Banking private key;
    - a state file with `version: 2`.
  - Command-scoped cases (contracts/config.md table):
    - a missing Firefly token file fails `accounts` and `check` but not `auth`;
    - a missing SMTP password or Telegram token file fails `check` but not `check --stdout`, `auth` or
      `accounts`;
    - an unwritable `log_file` directory fails `check` only, and `auth` and `accounts` still succeed.
  - A secret file with mode 0644 produces a WARN and continues.
- [x] T075 [US4] Implement fail-fast validation ordering in `internal/app/cli.go` and `internal/app/wire.go`:
  config load, then validation, then state load, all before building any client. Make T074 pass.
- [x] T076 [US4] Write the US4 acceptance test `internal/app/acceptance_us4_test.go` covering spec US4 scenarios 1–5.

**Checkpoint**: A quiet run always means "verified complete". All four stories are done.

---

## Phase 7: Polish & Cross-Cutting Concerns

- [x] T077 [P] Write `PrivacySuite` in `internal/app/privacy_test.go` (SC-009). Run a full `check` with fixtures
  containing IBANs, a Firefly token, a Telegram token and an SMTP password. Then assert that the file log,
  stderr, the Telegram request bodies and the email DATA contain **no** secret and **no** unmasked IBAN.
  Also assert that the file log at `log_level: debug` contains **no** transaction description or counterparty
  name from the fixtures. Descriptions may appear only in the digest, and on stderr only when every delivery
  failed (FR-028).
- [x] T078 [P] Add a `ReadOnlyGuaranteeSuite` in `internal/app/readonly_test.go` (SC-003). Across a full `check`,
  `accounts` and a failure run, the Firefly fake records only GET requests and the Enable Banking fake records
  no payment-path requests (`/payments`).
- [x] T079 [P] Create `config.example.yaml` at the repo root, copied from contracts/config.md with placeholder
  values only.
- [x] T080 [P] Write `README.md` for users:
  - what the tool does, and that it is a reminder, not an importer;
  - install (`task build`) and the configuration reference (link `config.example.yaml`);
  - `auth`, `accounts --ids` and `check --stdout` walkthroughs;
  - cron with `flock` every 6 hours (PSD2 note);
  - a logrotate example (`copytruncate` not needed, since each run reopens the log);
  - an exit code table;
  - troubleshooting for consent expiry and "ambiguous mapping".
  - Never mention other projects or local paths.
- [x] T081 Run `task check`, `task audit` and a coverage report into `cover.out`
  (`go test ./... -coverpkg=./... -coverprofile=cover.out`, as defined in `Taskfile.yml`). Target at least 80% total, and investigate any
  package below that, especially `reconcile`, `mapping` and `firefly`.
- [ ] T082 Run the `specs/001-missing-tx-reminder/quickstart.md` validation against the owner's real instances: sections 3–8, including the
  failure drills table. Also time one real `check` (`/usr/bin/time -v firefly-jar check --stdout`) and compare
  the wall-clock time with SC-008 (under 2 minutes for up to 10 accounts and 30 days). Record the observed
  results in the PR description, not in tracked files.
- [x] T083 Constitution compliance review against `.specify/memory/constitution.md` before finish-branch:
  - Walk Principles I–VI and the Development Workflow checks.
  - No Firefly write path.
  - No secret or PII in logs.
  - No silent-drop path in reconcile; the invariant is asserted in every case.
  - Reviewers use `superpowers:requesting-code-review`.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: none. T006 depends on T001.
- **Foundational (Phase 2)**: depends on Setup and **blocks all stories**. Within it:
  - T009 depends on T007, and T010 on T008;
  - T012 on T011;
  - T015 on T013 and T014, and uses domain and redact;
  - T017 on T016 and T012;
  - T019 on T018;
  - T022 on T020 and T021;
  - T024 on T023 and T009/T010;
  - T025 on T009/T010.
- **US1 (Phase 3)**: depends on Foundational. This is the MVP.
- **US2 (Phase 4)**: depends on US1's `app` pipeline (T052, T054) and the enablebanking client (T030).
- **US3 (Phase 5)**: depends on US1 mapping (T038) and CLI (T054). It is independent of US2.
- **US4 (Phase 6)**: depends on US1's `check` pipeline. It is independent of US2 and US3, though its suites
  must keep them green.
- **Polish (Phase 7)**: depends on all stories.

### Within US1

- The enablebanking chain is T026 → T027 and T028/T029 → T030.
- The Firefly chain is T031 → T032, then T033/T034/T035 → T036.
- Mapping is T037 → T038, and reconcile is T039 → T040. Both depend only on the foundation (T024, T025), so
  they can start alongside the Firefly client track.
- Report is T041 → T042 (needs T038, T040). Digest is T043 → T044 (needs T042).
- Notify is T045 → T046 (needs T044). Telegram (T047 → T048) and email (T049 → T050) each need T046.
- App: T051 → T052 needs everything above. T053 → T054 needs T052. T055 and T056 come last.

### Parallel Opportunities

- **Setup**: T002–T005 run in parallel.
- **Foundational**: all test tasks (T007, T008, T011, T013, T014, T016, T018, T020, T021, T023) can be written in
  parallel. The implementations then follow their own test.
- **US1**: four independent tracks after the foundation:
  - (a) enablebanking T026–T030;
  - (b) Firefly T031–T036;
  - (c) mapping and reconcile T037–T040 (they need only the foundational types T024 and T025);
  - (d) Telegram and email T047–T050 once T046 is done.
- **Across stories**: US3 and US4 can proceed in parallel with US2 after US1.

---

## Parallel Example: User Story 1

```bash
# After Phase 2, dispatch the independent test-first tracks together:
Task: "T026 SignerSuite in internal/bank/enablebanking/jwt_test.go"
Task: "T031 ReadOnlySuite in internal/firefly/readonly_test.go"
Task: "T037 AutoSuite in internal/mapping/mapping_test.go"
Task: "T039 ReconcileSuite in internal/reconcile/reconcile_test.go"

# Once T046 (notify interface) lands:
Task: "T047 TelegramSuite in internal/notify/telegram/telegram_test.go"
Task: "T049 EmailSuite in internal/notify/email/email_test.go"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Phase 1 Setup, then Phase 2 Foundational.
2. Phase 3 (US1), then **stop and validate** with T056 and quickstart §5–§6, using a hand-provisioned
   `state.json`.

### Incremental Delivery

1. US1: reminders work (MVP).
2. US2: self-service `auth` and consent warnings. There's no need to hand-edit state any more.
3. US3: overrides and `accounts`, so every account is covered.
4. US4: hardened failure behavior.
5. Polish: privacy and read-only guarantee suites, README, quickstart validation.

### Superpowers execution (Principle III)

Run the whole list in the `feature/001-missing-tx-reminder` worktree:
`superpowers:using-git-worktrees` → per task `superpowers:test-driven-development` (red-green-refactor) →
`superpowers:subagent-driven-development` for the parallel tracks → `superpowers:requesting-code-review` →
`superpowers:finishing-a-development-branch`.

---

## Notes

- Tasks marked [P] touch different files and have no incomplete dependencies.
- Every test task must be **seen failing for the expected reason** before its implementation task starts.
- Commit after each red-green-refactor cycle or logical group, using Conventional Commits with no AI trailers.
- Do not add dependencies beyond `github.com/goccy/go-yaml` and `github.com/stretchr/testify` without approval.
