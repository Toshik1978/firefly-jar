# Phase 0 Research: Missing Transaction Reminder

All Technical Context unknowns are resolved below. Sources were checked 2026-09-22.

## R1. OpenAPI code generation vs hand-written models

- **Decision**: Hand-write small, value-typed structs for both APIs, covering only the fields we use. Do not add
  oapi-codegen. Record the reference spec versions (Firefly III `v6.7.2` v1, Enable Banking current
  `enablebanking-api.yaml`) in the adapter package doc comments. Contract tests run against anonymized JSON
  fixtures shaped like real responses.
- **Rationale**: We use about 15 Firefly III fields and about 20 Enable Banking fields across 5 endpoints.
  Generated Firefly models were 1137 lines of pointer-heavy code (`TransactionSplit` has about 80 fields) with
  enum quirks (`AccountRolePropertyLessThannil`), and the tool pulls kin-openapi and others into `go.mod`. We
  would still have to hand-write the read-only HTTP clients. Hand-written structs match the constitution's
  stdlib-first rule (Principle VI).
- **Alternatives considered**: oapi-codegen v2.8.0 models-only with `include-operation-ids`. It works for both
  specs (Firefly is OpenAPI 3.0.0; Enable Banking 3.1.0 generates cleanly), and it stays available if drift
  becomes a problem. We rejected generating a full client because it would put write endpoints in the codebase
  (Principle I).

## R2. Enable Banking: API authentication

- **Decision**: Every request carries `Authorization: Bearer <JWT>`. The JWT header is
  `{"typ":"JWT","alg":"RS256","kid":<app_id>}` and the claims are
  `{"iss":"enablebanking.com","aud":"api.enablebanking.com","iat":now,"exp":now+3600}`. It is signed with
  `crypto/rsa.SignPKCS1v15` over SHA-256 and reused until 5 minutes before `exp`.
- **Rationale**: RS256 is the only supported algorithm and the maximum lifetime is 24 hours. The standard
  library is enough, so no JWT library is needed.
- **Alternatives considered**: golang-jwt was rejected as an unnecessary dependency.

## R3. Enable Banking: authorization flow (`auth <bank>`)

- **Decision**:
  1. `GET /aspsps?country=<cc>&psu_type=<type>&service=AIS`: find the bank by name and read
     `maximum_consent_validity` (seconds).
  2. `POST /auth` with `access.valid_until = now + maximum_consent_validity − 1h` (safety margin),
     `access.transactions=true`, `access.balances=false`, `aspsp{name,country}`, a random 128-bit `state`
     (hex), the configured `redirect_url`, and `psu_type`.
  3. Print `url`. Read one line from stdin and parse it as a URL. If it has `error`, fail with
     `error_description`. If `state` does not match, fail. Otherwise take `code`.
  4. `POST /sessions {"code":…}`. Store `session_id`, `access.valid_until`, and for each account: `uid`,
     `identification_hash`, `account_id.iban` (may be absent), `currency`, `name`.
  5. Write the state atomically (R11). Only after that succeeds, optionally `DELETE /sessions/{old_id}` for
     the replaced session. This is best effort, and the errors are logged.
- **Rationale**: This is the flow the docs define. Some banks allow only one session per user, so
  re-authorizing can kill the old session. That is why the new session is saved first.
- **Alternatives considered**: a built-in HTTP callback server was ruled out in brainstorming (Principle VI).

## R4. Enable Banking: identifiers used for account overrides

- **Decision**: Configuration overrides for accounts without an IBAN use **`identification_hash`**, not `uid`.
  `accounts --ids` prints full hashes for copy-paste. Normal output truncates them.
- **Rationale**: `uid` is valid only while its session is authorized, so it changes after every `auth`.
  `identification_hash` is stable across sessions for the same account.
- **Alternatives considered**: `uid` breaks on every consent renewal. Account name is not unique and banks can
  rename accounts.

## R5. Enable Banking: fetching and normalizing transactions

- **Decision**:
  - `GET /accounts/{uid}/transactions?date_from=<window_start − 1 day>` with no status filter and no
    `date_to`. Follow `continuation_key` until it is null or absent. A page can be empty and still carry a
    key. The date is widened by one day because the provider interprets dates in UTC, and the result is
    filtered locally using the configured time zone.
  - Amount: `abs(transaction_amount.amount)` parsed as an exact decimal, with the sign taken from
    `credit_debit_indicator` (`DBIT` → −, `CRDT` → +).
  - Date (FR-005a): `transaction_date` ?? `booking_date` ?? `value_date`. If none are present, the account is
    unchecked with reason "bank data incomplete". The tool never guesses.
  - Status mapping:

    | Provider `status` | Normalized | Treatment |
    |---|---|---|
    | `BOOK` | booked | reconciled |
    | `PDNG`, `HOLD`, `OTHR` | pending | reconciled and flagged pending (err on the side of reporting) |
    | `CNCL`, `RJCT` | void | not reconciled; counted in the run summary as `void` |
    | `SCHD` | void | future/scheduled, not a transaction yet; counted as `void` |
    | any, amount 0 | void | not a money movement; counted as `void` |

  - Description: `creditor.name` for DBIT or `debtor.name` for CRDT, else the first
    `remittance_information` line, else `(no description)`.
  - Response shape: accept both the flat `{"transactions":[…]}` form (status per transaction) and the grouped
    `{"transactions":{"booked":[…],"pending":[…]}}` form, where the group implies `BOOK` or `PDNG`.
  - If `credit_debit_indicator` is missing (it is required by the spec but not always present), the sign of
    `amount` itself is used.
  - Zero-amount entries (card verifications, holds released) are `void`: counted, not reconciled.
  - Pagination safety cap: 100 pages per account. Hitting it makes the account unchecked ("bank data
    incomplete"). `continuation_key` is valid only within the current session.
- **Rationale**: Only `transaction_amount`, `credit_debit_indicator` and `status` are guaranteed fields, and the
  sign convention for `amount` is undocumented, hence `abs` plus the indicator. Cancelled and rejected entries
  are not money movements. Reporting them would be noise the owner cannot fix in Firefly III.
- **Alternatives considered**: requesting `transaction_status=BOOK` and `PDNG` separately would double the
  requests against the PSD2 daily limit (R7).
- **Spec impact**: FR-011's list of states gains "void (cancelled/rejected/scheduled at the bank)". This is
  applied to spec.md.

## R6. Linking pending to booked (FR-005b)

- **Decision**: Deduplicate only by **`entry_reference`**: a `PDNG`/`HOLD`/`OTHR` entry and a `BOOK` entry on
  the same account with the same non-empty `entry_reference` → keep the booked one and count
  `deduplicated`. `transaction_id` is never used.
- **Rationale**: The Enable Banking FAQ says `entry_reference` is unique and stable within an account.
  `transaction_id` "can change between fetches". Most banks omit `entry_reference` on pending entries, so for
  those banks both copies are kept, which is the owner's accepted behavior (Clarification Q5).
- **Alternatives considered**: amount+date heuristics were rejected by the owner (daily coffee case).

## R7. Enable Banking: rate limits, errors, consent expiry

- **Decision**:
  - HTTP 429 (`ASPSP_RATE_LIMIT_EXCEEDED`) → retry per R12. If still limited, the account is unchecked with
    reason "rate limited".
  - Error codes `EXPIRED_SESSION`, `REVOKED_SESSION`, `CLOSED_SESSION` (any HTTP status) → every account of
    that bank is unchecked with reason "consent expired" or "consent revoked", with a re-auth hint.
  - Before any call, if `valid_until <= now` → the bank is unchecked with reason "consent expired". If it is
    within `consent_warn_days`, add a consent warning.
  - Error body `{message, code?, error?, detail?}`: only `error` and `message` are used in reasons, and both
    pass through the redactor.
  - `Psu-*` headers are never sent. The tool runs unattended, and sending them would misrepresent that.
- **Rationale**: Unattended access is typically capped at 4 fetches per day per account. The quickstart says
  cron should run at most every 6 hours.

## R8. Firefly III API (read-only)

- **Decision**:
  - **Base URL**: accept `https://host`, `…/api` or `…/api/v1` in config and normalize to `…/api/v1`. The
    token is trimmed, because a trailing newline in a token file breaks the header.
  - **Headers**: `Authorization: Bearer <PAT>` and `Accept: application/json`. `Accept` is required: without
    it, 401 and 404 responses are not JSON, and an unexpected value gets a 406-style error. Successful bodies
    arrive as `application/vnd.api+json`, and the decoder accepts either type. The Go transport negotiates
    gzip on its own. No `X-Trace-Id` is sent.
  - **Redirects are never followed** (`CheckRedirect` returns `http.ErrUseLastResponse`), so the bearer token
    is never replayed to another host. A 3xx is treated as a Firefly error.
  - **Accounts**: `GET /api/v1/accounts?type=asset&limit=500&page=N`. This includes credit cards modelled as
    asset accounts with role `ccAsset`. Liability accounts are not mapping targets. Fields used: `id`
    (string), `attributes.name`, `account_role`, `active` (a missing value counts as active), `iban`,
    `currency_code`, `currency_decimal_places`.
    - `iban`: Firefly already strips whitespace; the client uppercases it. It is `null` when empty.
    - `currency_code` is never null. Without an explicit account currency it falls back to the primary
      currency.
    - Inactive accounts are not eligible for auto-mapping, but overrides may target them.
  - **Transactions**: `GET /api/v1/accounts/{id}/transactions?start=<ws − tol>&end=<today + tol>&types=withdrawal,deposit,transfer&type=default&limit=500&page=N`.
    - Since v6.4.5 the server reads `types`, not `type`. Sending both works on old and new servers. Unknown
      values fall back to `all`, so the client **also filters by split `type`** and keeps only `withdrawal`,
      `deposit` and `transfer`. `type` is decoded as a plain string, because an unknown value such as
      `liability credit` must not fail the page.
    - `start` and `end` are inclusive whole days and must differ (`start == end` returns a 422). Our range is
      always longer than a day.
  - **Pagination**:
    - Count pages yourself from 1 to `meta.pagination.total_pages`, and never trust `current_page` to advance.
      A missing `meta.pagination` means a single page.
    - The server paginates **split rows, not groups**, and returns only the splits that touch account A. The
      same group id can therefore appear on two consecutive pages with different splits. The client merges
      splits by group id, then by `transaction_journal_id`, across pages, which rebuilds A's view of each
      group exactly.
    - Safety cap: 200 pages per account. Hitting the cap makes the account unchecked ("Firefly data
      incomplete"), never a silent truncation.
  - **Split → amount in A's currency** (`amount` is always positive; decimal strings have up to 12 places,
    e.g. `"12.340000000000"`, and are parsed exactly):
    - if `currency_code == A.currency` → use `amount`;
    - else if `foreign_currency_code == A.currency` and `foreign_amount` is non-empty → use `foreign_amount`.
      This is the destination side of a cross-currency transfer, or data recorded before an account's
      currency changed;
    - else the split is not comparable in A's currency. It is skipped and logged at DEBUG with the group id,
      `account_id` (the Firefly III account id, an internal number rather than a bank identifier), date and
      amount only, never the description; the run's logger is passed to the client, so the record reaches
      `log_file` at `log_level: debug`. A bank transaction it might have matched will surface as missing,
      which errs on the side of reporting.
    - Sign: `source_id == A` → negative; `destination_id == A` → positive.
  - **Group → FireflyEntry**: sum the signed amounts of A's comparable splits in the group into one entry
    (FR-009). The date is the calendar-date prefix (`YYYY-MM-DD`) of the first counted split's `date` as sent. Firefly
    renders dates in its own configured time zone, which is the date the owner typed in the UI, so the offset
    is not converted. The date tolerance absorbs any server/tool time-zone mismatch.
- **Rationale**: Verified against the Firefly III source and API behavior:
  - the `types` parameter rename;
  - per-split pagination and group re-assembly;
  - amounts and currency taken from the source side;
  - dates rendered in the server's time zone;
  - no API token scopes. A personal access token is full read/write, which is why FR-001 is enforced in code.
- **Read-only enforcement (FR-001)**: the Firefly client is built on an `http.RoundTripper` wrapper that returns
  an error for any method other than `GET` before the request leaves the process (constitution §I: the client
  "MUST issue only `GET` requests"; `HEAD`, `OPTIONS` and extension methods are refused too). The client
  package exposes only `ListAccounts` and `ListAccountTransactions`. Tests confirm that
  POST/PUT/PATCH/DELETE, HEAD, OPTIONS, TRACE, CONNECT and an extension method through the transport fail and
  never reach the base transport, an `httpmock` transport whose no-responder counts every request that
  reaches it, and that a redirect is not followed.
- **Errors**: 401 (or a 403, e.g. from a proxy) means "Firefly token rejected", reported in the digest as
  "Firefly III unauthorized" rather than "unreachable", and 404 means "account #N not found". Both come as
  `{"message","exception"}`. A 422 has `{"message","errors"}`. 5xx, network errors and 429 (only ever from a
  reverse proxy, since Firefly has no API throttle) are retried per R12. 4xx responses other than 429 are
  never retried. A non-JSON body is reported by status code only.

## R8a. Liability accounts (deferred)

- **Decision**: Firefly III liability accounts are **not** mapping targets in this feature (owner decision,
  2026-09-22). A bank card that is modelled as a liability in Firefly III shows as `unmapped` and can be
  `exclude`d.
- **Rationale**: The owner's cards are asset accounts. The signed-amount rule would work unchanged for
  liabilities, so adding them later only means adding a second account list call (`type=liabilities`) and
  widening FR-014.

## R9. Telegram notifier

- **Decision**: `POST https://api.telegram.org/bot<token>/sendMessage` with a JSON body
  `{chat_id, text, link_preview_options:{is_disabled:true}}`, **plain text (no `parse_mode`)**. Split on line
  boundaries at 4000 UTF-16 code units, below the 4096 limit, counted with `utf16.RuneLen`. Send parts to one
  chat sequentially, at least 1.1 s apart. Retry only on 429 (honoring `parameters.retry_after`) and on 5xx.
  401, 403 and 400 fail that recipient immediately. The bot token must never appear in logs, and since the URL
  contains it, URL errors are redacted.
- **Rationale**: Plain text means an arbitrary bank description can never trigger a "can't parse entities"
  error. Limits are about 1 message per second per chat.
- **Alternatives considered**: HTML `parse_mode` would need escaping everywhere for little benefit.
  MarkdownV2 is error-prone.

## R10. Email notifier

- **Decision**: stdlib `net/smtp` with explicit TLS:
  - Port 587: dial with a timeout, then `smtp.NewClient`, `Hello`. If `STARTTLS` is not offered, fail. Then
    `StartTLS(&tls.Config{ServerName: host, MinVersion: TLS12})`.
  - Port 465: `tls.DialWithDialer`, then `NewClient`.
  - Then `Auth(smtp.PlainAuth("", user, pass, host))`, and `Mail`/`Rcpt` for each recipient in one
    transaction. A rejected recipient is logged and the others continue.
  - Message: CRLF line endings; headers `From`/`To` via `mail.Address`, `Subject` via
    `mime.QEncoding.Encode("utf-8", …)`, `Date` (RFC1123Z), a random `Message-ID`, `MIME-Version: 1.0`,
    `Content-Type: text/plain; charset=utf-8`, `Content-Transfer-Encoding: quoted-printable`. Header values
    containing CR or LF are rejected. A connection deadline covers the whole session.
- **Rationale**: `net/smtp` is frozen but works. `smtp.SendMail` is avoided because it upgrades to TLS only if
  the server offers it and has no timeout.

## R11. State file and atomic writes

- **Decision**: JSON with a `version` field. Writes use `os.CreateTemp(dir, ".state.*.tmp")`, `Chmod(0600)`,
  write, `Sync`, `Close`, `Rename`, then an fsync of the directory. The temp file is removed on any error. On
  load, a file with group/other permission bits produces a WARN.
- **Rationale**: FR-019 requires crash safety and owner-only permissions.

## R12. Retries and timeouts

- **Decision**: One shared `http.RoundTripper` retry wrapper (not used by Telegram, which has its own 429
  semantics). It retries at most 3 times on network errors, 5xx and 429, with exponential backoff of 1 s, 2 s,
  4 s plus up to 20% jitter. Other 4xx responses are never retried. A `Retry-After` header (seconds or
  HTTP-date) replaces the backoff and is capped at 60 s. If the requested wait is longer than the cap, the
  tool gives up and the account becomes unchecked ("rate limited"). Only the idempotent, bodyless GET and HEAD
  requests are retried (the Firefly client sends GET only). The interactive `auth` calls (`POST /auth`,
  `POST /sessions`) are never retried, because the authorization code can be used only once. The timeout
  is per attempt: each attempt, its response body read included, is bounded at 30 s, and a retry (with the
  backoff or `Retry-After` wait before it) starts a fresh 30 s, so a 45 s `Retry-After` is waited out rather
  than cut short. There is no timeout across one request's retries; the whole `check` (or `accounts`) run is
  capped at 10 minutes by its context, and that cap bounds the waits too. Telegram gets the same 30 s
  per-attempt bound without the retry layer.
- **Implementation** (owner decision, 2026-09-23): the attempt loop and the wait between attempts are
  `github.com/avast/retry-go/v5` (MIT, no runtime dependencies). `httpclient.RetryTransport` keeps every HTTP rule
  above in its own code, because retry-go knows nothing about HTTP:
  - Requests other than bodyless GET and HEAD never enter retry-go and go straight to the timeout layer.
  - Each attempt turns a network error, 5xx or 429 into a typed error that carries any `Retry-After` wait.
    A custom `DelayType` reads that wait, or else computes the 1 s, 2 s, 4 s backoff with an injectable ±20%
    jitter. retry-go's own jitter only adds to the delay, so it cannot express ±20%.
  - A `Retry-After` over the cap fails the attempt with `retry.Unrecoverable`, and callers still find
    `*RetryAfterTooLongError` with `errors.As`.
  - retry-go returns no value alongside its final error. The wrapper therefore counts attempts itself and
    returns the final attempt's retryable response as a success, so the caller still gets the last status
    and body.
  - `LastErrorOnly(true)` keeps retry-go from joining every attempt's error into one.
  - `retry.Context` stops the wait when the request's context ends. It also checks the context before the
    first attempt, so a request whose context has already ended is never sent. The hand-written loop made
    that one attempt anyway.
  - A request whose context ends before the first attempt or during a wait fails with
    `httpclient: request context ended`, wrapping the context's `context.Cause` (which is `ctx.Err()` unless
    a cause was set). The hand-written loop reported `wait interrupted` with `ctx.Err()`. `errors.Is` finds
    the context's error either way.
  - Discarded bodies are drained (at most 64 KiB) and closed by the wrapper.
- **Testing**: `testing/synctest` (GA since Go 1.25) makes the backoff and `Retry-After` tests run instantly
  and deterministically. retry-go waits on `time.After`, which follows the bubble's fake clock. Guard tests
  pin two behaviours that the library's defaults would change: giving up reports only the last attempt's
  error, and retrying writes nothing to stderr.
- **Alternatives considered**:
  - The original hand-written loop and timer, which used only the stdlib. retry-go replaced it by owner
    decision. The HTTP rules did not change, and the tests that pinned them pass unchanged. The two
    differences are the ones listed above: no attempt at all once the context has ended, and the wording of
    the cancellation error.
  - `github.com/hashicorp/go-retryablehttp` was rejected. Its defaults conflict with this decision and the
    project invariants:
    - It logs every request to stderr through a global logger, and cron mails any output.
    - It retries POST and replays the body.
    - It honours `Retry-After` only on 429 and 503, with no cap, and parses only one date format.
    - It follows redirects in a nested `http.Client`.
    - It puts the full URL in its errors.
    - When it gives up it drops the last response. Handing that response back makes `net/http` log a
      warning to stderr.

    Each of these would need a hook of our own and a test to pin it. It also pulls in
    `github.com/hashicorp/go-cleanhttp` v0.5.2 (released 2021), which fails the release-date rule. It is
    MPL-2.0 and pre-1.0.

## R13. YAML parser

- **Decision**: `github.com/goccy/go-yaml` v1.19.x with strict decoding (`yaml.Strict()` /
  `yaml.DisallowUnknownField()`). It is one of a small, owner-approved set of third-party runtime
  dependencies (R15, R19), and the only one used for config decoding.
- **Rationale**: It is actively maintained, has a strict mode that rejects unknown keys (FR-030), and gives
  error messages with line and column, which matters for a human-edited config. It is also the parser already
  standard across the author's Go tooling, so there is one fewer library to learn.
- **Alternatives considered**: `go.yaml.in/yaml/v3` (the maintained continuation of the archived
  `gopkg.in/yaml.v3`, with `KnownFields(true)`) is viable but gives weaker error positions.
  `go.yaml.in/yaml/v4` is still a release candidate.

## R14. Logging (FR-038, FR-039)

- **Decision**: `log/slog` with `slog.NewMultiHandler` (Go 1.26+): a JSON handler that appends to `log_file`
  at `log_level`, plus a text handler on stderr at WARN and above. All attributes pass through a
  `ReplaceAttr` redactor: it masks IBAN-shaped strings and drops known secret keys. The run summary is logged
  at INFO, so it goes to the file only. Rotation is the host's job (logrotate `copytruncate`, or the file is
  reopened per run, which it always is because each run is a new process).

## R15. Money and dates

- **Decision**:
  - `Amount{Value decimal.Decimal, Currency string}` is backed by `github.com/shopspring/decimal` v1.4.0
    (owner-approved 2026-09-22) and parsed directly from decimal strings with no float, through a grammar
    check stricter than `decimal.NewFromString` alone: only an optional leading sign, digits, and at most
    one decimal point are accepted, so scientific notation, thousands separators and stray whitespace are
    all rejected. `decimal.Decimal`'s own normalization means Firefly's 12 places (`"12.340000000000"`)
    and a bank's 2 places (`"12.34"`) compare equal; `Equal` also compares currency.
  - More than 18 significant digits fails parsing, which makes the account unchecked ("bank data
    incomplete" or "Firefly data incomplete").
  - Display uses the currency's usual minor units: the Firefly account's `currency_decimal_places` when
    known, else 2 (`Amount.Format(decimalPlaces)`, backed by `decimal.Decimal.StringFixed`, which is
    `big.Int`-backed so padding to a large precision never overflows).
  - Dates are a civil `Date{Year, Month, Day}` (`internal/civil.Date`, R19). Bank dates are already
    calendar dates. Firefly dates are the `YYYY-MM-DD` prefix as rendered by the server (R8). "Today" and
    the window are computed once per run in the configured time zone. Day arithmetic uses `AddDays` and
    `DaysSince` (the signed day count between two dates); an earlier hand-rolled `DaysBetween` was renamed
    to `DaysSince` when `internal/civil` replaced the original hand-rolled date type (R19).
- **Rationale**: Exact comparison (FR-007), no hard-coded ISO 4217 table, and immunity to the two sides'
  different decimal precision. `shopspring/decimal` replaced a hand-rolled `Amount{Minor int64, Scale
  uint8, Currency string}` once the owner widened the dependency policy (2026-09-22): it is well-tested and
  removes bespoke normalization and rounding code.
- **Alternatives considered**: the original hand-rolled minor-units/scale representation worked but
  reimplemented decimal normalization and rounding that `shopspring/decimal` already provides correctly.

## R16. Go toolchain features used

- Go 1.27.1 (from `go.mod`): `testing/synctest`, `slog.NewMultiHandler`, `os.Root` (optional, not required).
  `encoding/json` (v1) is enough; `json/v2` is not needed.

## R17. Tooling, tests and repository conventions

- **Decision**:
  - **Task runner**: `go-task` (`Taskfile.yml`) with `setup`, `format`, `format:check`, `lint`, `test`
    (`go test -race ./...`), `build`, `check` (format:check → lint → test), `audit` (govulncheck), `clean`.
  - **Pinned tooling**: `mise` (`.mise.toml`: `go = "1.27"`, `golangci-lint = "2.13.2"`; CI's golangci-lint-action
    pins the same golangci-lint release).
  - **Lint**: golangci-lint v2 with the author's standard strict configuration, committed as `.golangci.yml`
    **verbatim**. Only the module path (gci prefix, gofumpt `module-path`) is adapted. Changes to it need
    explicit approval, the same as a dependency. It uses `default: none` plus an explicit linter list,
    including `wrapcheck`, `ireturn`, `gochecknoglobals`, `sloglint`, `revive` with `enable-all-rules`,
    `funcorder` and `wsl_v5`. Formatters are gofumpt (extra rules named individually), gci with the module
    prefix, and golines at 120 columns. gofumpt is a superset of `gofmt` and `govet` runs inside
    golangci-lint, so the constitution's gofmt and go vet gates are met by `task check`.
  - **Tests**: `github.com/stretchr/testify` suites, **test-only**. One `Test<Package>` entry point per package
    that only calls `suite.Run`, and all assertions in suite methods. Table-driven subtests use `s.Run`.
  - **Hooks**: `pre-commit` with `conventional-pre-commit` (commit-msg), `golangci-lint fmt --diff`
    (pre-commit) and `task check` (pre-push).
  - **CI**: `.github/workflows/ci.yml` runs `task check` plus govulncheck on Linux, and
    `commit-lint.yml` checks Conventional Commits.
  - **Branches and commits**: feature branches use the `feature/` prefix (this feature:
    `feature/001-missing-tx-reminder`). Conventional Commits, with no AI or co-author trailers.
- **Rationale**: These give one reproducible gate locally and in CI. The strict linter set enforces the
  constitution's style expectations mechanically (wrapped errors, injected loggers, no globals).
  testify suites keep test organization uniform. It is a test-only dependency and never reaches the binary,
  which is justified here per Principle VI.
- **Alternatives considered**: plain `go test` with the stdlib `testing` package would avoid the dependency,
  but it was rejected for consistency with the suite-based conventions in `.claude/CLAUDE.md`. A Makefile was
  rejected in favor of go-task.
- **HTTP test doubles** (owner decision, 2026-09-23):
  - **Decision**: `github.com/jarcoal/httpmock` v1.4.2 (MIT, released 2026-07-28), **test-only**, like
    testify. Every HTTP adapter test (`firefly`, `bank/enablebanking`, `notify/telegram`, `httpclient`)
    injects its own `httpmock.NewMockTransport()` into the client under test, never `httpmock.Activate` or
    the global `http.DefaultTransport`, so tests stay parallel-safe. Where a test counts requests, it
    registers a no-responder, because httpmock does not count a request that matches no responder.
    `internal/app` keeps `httptest.Server`: `app.RunEnv` and `BuildDeps` build their own real
    `http.Client`s and have no test-visible transport seam.
  - **Rationale**: One library replaces the recording, scripting and call-counting code each package had
    written for its own fake servers and `http.RoundTripper`s. It is in-process and opens no sockets, so it
    works inside a `testing/synctest` bubble, where an `httptest.Server` would hang the fake clock. When the
    request's context can end, it runs the responder on a goroutine of its own, so a test reads what the
    responder recorded only after `synctest.Wait()`. The package imports only the standard library. Its own
    test dependencies, `github.com/maxatome/go-testdeep` and `github.com/davecgh/go-spew`, appear in
    `go.sum` for module-graph verification but are never compiled into this module's build or test
    binaries. It never reaches the binary, which is justified here per Principle VI.
  - **Alternatives considered**: `httptest.Server` plus hand-written fake `http.RoundTripper`s, which used
    only the stdlib. They were kept only where a real listener is the point (`internal/app`), because the
    rest repeated the same scripting code in every package and a server cannot run inside a synctest bubble.

## R18. Reporting ambiguity instead of guessing (constitution §II, FR-025a)

- **Decision**: Keep the deterministic greedy pairing (FR-008). It produces the correct *count* of missing
  transactions for every bucket, because all entries in a bucket have the same amount. On top of it, attach a
  "possible match" hint to each missing item: a same-amount Firefly entry that was taken by another bank
  transaction within tolerance, or one that lies within 2× the tolerance.
- **Rationale**: With two same-amount purchases and one entry, any pairing is a guess about *which* purchase
  is missing. The hint turns that guess into reported information, as §II requires, without adding a
  "maybe matched" state that would weaken "a quiet run means verified complete".
- **Alternatives considered**:
  - Marking such transactions "ambiguous" instead of missing: rejected, because it would make the count
    fuzzy.
  - An optimal assignment by minimal total date distance: same count, still a guess, more code.

## R19. CLI parsing (cobra) and the vendored civil date

- **Decision**:
  - `internal/app` parses subcommands with `github.com/spf13/cobra` (owner-approved 2026-09-22) instead of
    `flag.NewFlagSet`. `Run(args, stdin, stdout, stderr) int` stays the tested entry point: a cobra root
    command wired with `SetArgs`/`SetIn`/`SetOut`/`SetErr` and `SilenceUsage`/`SilenceErrors`, with exit
    codes mapped explicitly (`--version`, an unknown subcommand, `check --help`, and so on).
  - `internal/civil` is a trimmed copy of `cloud.google.com/go/civil` v0.123.0's `Date` type (package
    `civil`, file `civil.go`; Apache-2.0 header kept). The `Time` and `DateTime` types, their
    `database/sql` `Scan`/`Value` integration, and `AddMonths`/`AddYears`/`Weekday` were removed because
    nothing in firefly-jar needs them. `ParseDate` and `UnmarshalText` now wrap the underlying error to
    satisfy `wrapcheck`. This replaces the original hand-rolled date type; the check window holds
    `civil.Date` and `DaysBetween` is replaced by `civil.Date.DaysSince`. The window type itself,
    `civil.Range`, is this repository's own code in `internal/civil/range.go`, beside the upstream copy.
- **Rationale**: `flag.NewFlagSet` cannot express `check [--stdout]`, `auth <bank>`, `accounts [--ids]` and
  global flags without hand-written subcommand dispatch and usage text; cobra does this directly. Vendoring
  `civil.Date` gives the same well-tested proleptic-Gregorian date arithmetic as depending on
  `cloud.google.com/go/civil` without pulling in the rest of the `cloud.google.com/go` module tree, which
  firefly-jar otherwise has no use for.
- **Alternatives considered**: `flag.NewFlagSet` and a hand-rolled date type were the original
  decisions, kept only as long as the dependency policy required stdlib-only code; the owner widened that
  policy on 2026-09-22. Depending on `cloud.google.com/go/civil` directly was rejected only because of its
  module tree, not the type itself.
