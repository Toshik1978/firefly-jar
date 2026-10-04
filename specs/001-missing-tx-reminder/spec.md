# Feature Specification: Missing Transaction Reminder

**Feature Branch**: `feature/001-missing-tx-reminder` (no git repository yet; branch to be created in the worktree step)

**Created**: 2026-09-22

**Status**: Draft

**Input**: User description: "firefly-jar: a read-only reconciliation checker that reminds the owner about bank
transactions missing from their Firefly III instance. It NEVER creates, edits or deletes anything in Firefly III
or at the bank; the owner enters transactions manually. It is not an importer." (Full list of approved design
decisions from the brainstorming session is reflected in the requirements below.)

## Clarifications

### Session 2026-09-22

- Q: Which bank date is compared with the Firefly III date and used for the check window? → A: Transaction
  (purchase) date if provided, otherwise booking date, otherwise value date.
- Q: Where do logs go, given cron emails any output? → A: Only warnings/errors to error output; full
  structured log (including run summary) appended to a configured log file.
- Q: Which Firefly III account does a bank account map to when several share an IBAN (multi-currency)? → A:
  Match on IBAN and currency. If still ambiguous, report as unchecked ("ambiguous mapping") until an override
  resolves it.
- Q: Who receives the digest? → A: Each channel accepts a list of recipients (Telegram chat IDs, email
  addresses). All recipients get the same full digest.
- Q: How are pending+booked copies of the same purchase handled? → A: Keep both (no amount/date heuristic,
  which would merge genuinely repeated purchases like a daily coffee). Deduplicate only when the bank
  provider marks both entries with the same transaction identifier.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Get reminded about transactions I forgot to enter (Priority: P1)

The owner keeps their household finances in Firefly III and enters every transaction by hand. Sometimes a card
payment, often one made by their spouse, never gets entered. A scheduled job on the owner's server compares
recent bank transactions with Firefly III. When bank transactions have no counterpart in Firefly III, the owner
receives a reminder digest (Telegram and/or email) listing them, grouped by account. The owner then enters them
manually. The reminder repeats on every run until each transaction is entered or ages out of the check window.

**Why this priority**: This is the whole reason the tool exists. Without it there is no value.

**Independent Test**: With one connected bank account, a mapped Firefly III account and a configured notifier,
leave one known bank transaction unentered and run a check. The digest lists exactly that transaction. After
the owner enters it and the check runs again, nothing is sent.

**Acceptance Scenarios**:

1. **Given** a bank transaction of −12.40 EUR on 2026-09-20 and a Firefly III withdrawal of 12.40 EUR on
   2026-09-21 on the mapped account, **When** a check runs with a 3-day tolerance, **Then** the transaction is
   treated as entered and no reminder is sent for it.
2. **Given** a bank transaction with no Firefly III transaction of the same amount and direction within the
   tolerance, **When** a check runs, **Then** the digest lists it with date, amount, currency and a short
   description, and the run ends with the "missing found" outcome.
3. **Given** two bank transactions of −5.00 EUR on the same day and only one matching Firefly III transaction,
   **When** a check runs, **Then** exactly one of them is reported missing.
4. **Given** a pending (not yet booked) bank transaction with no Firefly III counterpart, **When** a check
   runs, **Then** it appears in the digest flagged as pending.
5. **Given** a missing transaction dated on the first day of the check window, **When** a check runs, **Then**
   it is flagged as "last reminder".
6. **Given** every bank transaction in the window has a Firefly III counterpart and there are no warnings or
   problems, **When** a check runs, **Then** no notification is sent and the run ends with the "clean" outcome.
7. **Given** a missing transaction that was already reported on the previous run and is still not entered,
   **When** the next check runs, **Then** it is reported again.

---

### User Story 2 - Connect a bank and keep the connection alive (Priority: P2)

The owner connects each bank once by running an authorization command. The tool prints a bank login link, the
owner completes the bank login in a browser, then pastes the resulting redirect address back into the terminal.
The connection is saved. Bank consent expires after a bank-defined period. Before it does, the reminder digest
warns the owner to re-authorize. Once it has expired, the affected accounts are reported as unchecked.

**Why this priority**: Checks cannot see bank data without a connection. The initial connection could be
provisioned by hand for a first test, but without renewal warnings the tool would silently stop working after
consent expires.

**Independent Test**: Run the authorization command for a configured bank, complete the login and paste the
redirect address. The connection is saved and the bank's accounts become visible. Then simulate a consent that
expires within the warning period and confirm the digest contains a renewal warning.

**Acceptance Scenarios**:

1. **Given** a bank configured by name and country, **When** the owner runs the authorization command, **Then**
   a login link is printed and the tool waits for the pasted redirect address.
2. **Given** the owner pastes a redirect address whose anti-forgery value does not match the one issued,
   **When** the tool processes it, **Then** it refuses the address and saves nothing.
3. **Given** a successful authorization, **When** the tool saves the connection, **Then** the saved state is
   readable only by the owner's system user and replaces any previous connection for that bank.
4. **Given** a bank consent expiring in 5 days and a 7-day warning period, **When** a check runs, **Then** the
   digest includes a warning naming the bank and the command to re-authorize, even if nothing is missing.
5. **Given** an expired bank consent, **When** a check runs, **Then** that bank's accounts are listed as
   unchecked with reason "consent expired", other banks are still checked, and the run ends with the "check
   failed" outcome.

---

### User Story 3 - Trust that every account is actually covered (Priority: P3)

Bank accounts are linked to Firefly III asset accounts automatically by IBAN. Where that doesn't work (for
example a card account without an IBAN), the owner adds a mapping in configuration, or excludes an account
they don't track. An accounts command shows every bank account and what it maps to, so the owner can verify
coverage. Any bank account that is neither mapped nor excluded is reported as unchecked on every run.

**Why this priority**: The P1 reminder is only trustworthy if every account is covered. This story makes
coverage visible and fixable.

**Independent Test**: Connect a bank with two accounts, one whose IBAN exists in Firefly III and one without an
IBAN. Run the accounts command: the first shows as auto-mapped and the second as unmapped. Add a manual mapping
and rerun: both show as mapped.

**Acceptance Scenarios**:

1. **Given** a bank account whose IBAN (ignoring spaces and letter case) and currency equal those of exactly
   one Firefly III asset account, **When** the accounts command runs, **Then** it shows the pair as
   auto-mapped.
2. **Given** a multi-currency bank with EUR and USD accounts under one IBAN, and Firefly III asset accounts
   with that IBAN in EUR and USD, **When** the accounts command runs, **Then** each bank account maps to the
   Firefly III account of the same currency.
3. **Given** two Firefly III asset accounts with the same IBAN and currency, **When** a check runs, **Then** the
   bank account is listed as unchecked with reason "ambiguous mapping".
4. **Given** a configuration override mapping a bank account identifier to a Firefly III account, **When** the
   accounts command runs, **Then** the override takes precedence over IBAN matching.
5. **Given** a bank account marked as excluded, **When** a check runs, **Then** it is not checked and not
   reported as unchecked.
6. **Given** a bank account that is neither mapped nor excluded, **When** a check runs, **Then** the digest lists
   it as unchecked with reason "no Firefly III account mapped" and the run ends with the "check failed" outcome.

---

### User Story 4 - Know when the check itself failed (Priority: P4)

A quiet run must mean "everything verified". When something prevents a full check (Firefly III is unreachable,
a bank rate-limits or errors, a notification channel fails), the owner is told, and the scheduler sees a
distinct failure outcome it can alert on.

**Why this priority**: Without it, failures look like "nothing missing". It hardens P1 but is not needed for a
first working reminder.

**Independent Test**: Make Firefly III unreachable and run a check. A problem digest is sent and the run ends
with the "check failed" outcome. Make one of two notifiers fail. The other still delivers the digest.

**Acceptance Scenarios**:

1. **Given** Firefly III is unreachable after retries, **When** a check runs, **Then** a problem-only digest is
   sent and the run ends with the "check failed" outcome.
2. **Given** one bank returns a rate-limit response after retries and another bank works, **When** a check runs,
   **Then** the working bank's missing transactions are reported, the rate-limited bank's accounts are listed as
   unchecked with reason "rate limited", and the run ends with the "check failed" outcome.
3. **Given** Telegram delivery fails but email succeeds, **When** a digest is sent, **Then** the owner receives
   the email and the Telegram failure is logged.
4. **Given** every configured notifier fails, **When** a digest is sent, **Then** the digest is written to the
   error output and the run ends with the "check failed" outcome.
5. **Given** an invalid configuration (e.g. missing Firefly III address or unreadable secret file), **When** any
   command starts, **Then** it stops before contacting any external service, with a message naming the problem.

---

### Edge Cases

- **Split transactions**: a Firefly III transaction split into several parts on the same account is compared as
  the sum of its parts on that account.
- **Transfers between own accounts**: a transfer counts as money out on the source account and money in on the
  destination account. Each side is matched against its own bank account.
- **Window boundaries**: Firefly III transactions are considered up to the tolerance before the window start and
  after today, so a bank transaction near the edge still finds its counterpart.
- **Bank dates after today**: over a weekend or bank holiday, a bank can date a pending entry with the next
  business day. Such an entry is checked like any other (FR-004a) and shown with the bank's date.
- **Firefly III transactions with no bank counterpart** (cash, manual accounts, entries made in advance) are
  never reported.
- **Pending and booked copies of one purchase**: if the bank links them with the same transaction identifier,
  only the booked copy counts (FR-005b). Otherwise both are kept, and one extra reminder may appear until the
  bank drops the pending copy. This is accepted, because merging by amount and date would hide real repeated
  purchases.
- **Several same-amount candidates or a near miss**: pairing stays deterministic (FR-008). A missing
  transaction that had a competing or near-miss Firefly III entry shows a "possible match" hint (FR-025a), so
  the owner can tell which purchase is really missing.
- **Pending amount changes**: if the owner enters a pending amount and the booked amount later differs, the
  booked transaction is reported as missing. This is accepted behavior.
- **Foreign-currency card payments**: comparison uses the amount in the bank account's currency.
- **Long digests**: a digest longer than one Telegram message is split across several messages without losing
  or duplicating lines. Email sends one message.
- **Overlapping runs**: prevented by the scheduler setup (documented), not by the tool.
- **Empty window**: a bank account with no transactions in the window counts as checked and clean.
- **Paginated bank data**: all pages are fetched. If fetching stops partway, the account is unchecked, never
  treated as complete.

## Requirements *(mandatory)*

### Functional Requirements

**Read-only guarantee**

- **FR-001**: The system MUST NOT create, modify or delete any data in Firefly III. Every request it sends to
  Firefly III MUST be a read request, and this MUST be enforced so that a non-read request cannot be sent.
- **FR-002**: The system MUST access banks with read-only account-information consent and MUST NOT initiate
  payments or any other bank-side change.

**Check run**

- **FR-003**: The system MUST provide a `check` command that performs exactly one reconciliation run and exits.
  Scheduling is external (cron). The system MUST NOT run as a background service.
- **FR-004**: The check window MUST be the last N days up to and including today in a configured time zone
  (N configurable, default 30).
- **FR-004a**: A bank transaction dated after today (a bank may stamp a pending entry with the business day it
  expects to book it, which over a weekend or bank holiday is after today) MUST be checked as if it were in the
  window, never left out of it.
- **FR-005**: The system MUST retrieve both booked and pending bank transactions dated within the window, for
  every connected, mapped, non-excluded account, including all result pages.
- **FR-005a**: A bank transaction's date (used for the window, matching and the "last reminder" flag) MUST be
  its transaction (purchase) date when the bank provides one, otherwise its booking date, otherwise its value
  date.
- **FR-005b**: When the bank provider returns a pending entry and a booked entry carrying the same
  stable, per-account entry reference supplied by the provider (not a per-fetch transaction id) on the same
  account, the system MUST keep only the booked entry and
  count the dropped duplicate in the run summary. The system MUST NOT deduplicate by amount, date or
  description, so genuinely repeated purchases (e.g. the same coffee every day) are never merged.
- **FR-006**: The system MUST retrieve Firefly III transactions for each mapped account dated from (window start
  − tolerance) to (today + tolerance).
- **FR-006a**: A Firefly III transaction's date MUST be the calendar date Firefly III shows for it, meaning the
  date as entered by the owner, without re-converting time zones. Its amount MUST be the amount in the mapped
  account's currency: the foreign amount when that is the one in the account's currency (e.g. the receiving side
  of a cross-currency transfer). Opening-balance and reconciliation entries MUST NOT be used for matching.
- **FR-007**: A bank transaction MUST be considered entered when a Firefly III transaction on the mapped account
  has the same direction, exactly the same amount in the account currency, and a date within ± tolerance days
  (configurable, default 3). Amounts MUST be compared exactly, never with approximate numeric types.
- **FR-008**: Each Firefly III transaction MUST match at most one bank transaction. Matching MUST be
  deterministic: among bank and Firefly III transactions with the same account and signed amount, both ordered
  by date, each bank transaction pairs with the earliest unused Firefly III transaction within tolerance.
- **FR-009**: Split Firefly III transactions MUST be compared by the sum of their parts on the account.
  Transfers MUST be counted by their direction relative to the account.
- **FR-010**: Only bank transactions without a counterpart are reported. Firefly III transactions without a bank
  counterpart MUST NOT be reported.
- **FR-011**: Every bank transaction in the window MUST end in exactly one state: matched, reported missing,
  deduplicated as a pending copy (FR-005b), void (cancelled, rejected or scheduled at the bank, so not a money
  movement), or belonging to an unchecked account. None may be dropped silently. Deduplicated and void entries
  are counted in the run summary.
- **FR-012**: The system MUST NOT remember past reminders. A missing transaction is reported on every run until
  it is matched or leaves the window. There is no ignore or dismiss mechanism.
- **FR-013**: The `check` command MUST offer an option to print the digest to standard output instead of sending
  it to notifiers.

**Account mapping**

- **FR-014**: Bank accounts MUST be mapped to Firefly III asset accounts automatically when their IBANs are
  equal after removing spaces and ignoring letter case, **and** their currencies are equal. If more than one
  Firefly III account still qualifies, the bank account MUST NOT be mapped automatically. It is reported as
  unchecked with reason "ambiguous mapping" until a configuration override resolves it.
- **FR-015**: Configuration MUST allow mapping a bank account (by IBAN or by the bank provider's account
  identifier) to a specific Firefly III account, and excluding a bank account. Overrides take precedence over
  automatic mapping.
- **FR-016**: A bank account that is neither mapped nor excluded MUST be reported as unchecked on every run.
- **FR-017**: The system MUST provide an `accounts` command that lists every connected bank account with its
  masked IBAN or identifier, name, currency, and mapping status (auto-mapped, overridden, excluded, ambiguous,
  unmapped), without changing anything.

**Bank connection**

- **FR-018**: The system MUST provide an `auth <bank>` command for a configured bank. It requests consent for the
  longest validity the bank allows (minus a one-hour safety margin), prints a login link, accepts the pasted redirect address, verifies its
  anti-forgery value, and saves the resulting connection.
- **FR-019**: Saved connection state MUST be written so that a crash cannot leave a half-written file, and MUST
  be readable and writable only by the owning system user.
- **FR-020**: When a bank consent expires within a configured number of days (default 7), the digest MUST
  include a warning naming the bank and the command to re-authorize, even when nothing is missing.
- **FR-021**: When a bank consent has expired, that bank's accounts MUST be reported as unchecked with reason
  "consent expired".
- **FR-022**: The bank integration MUST sit behind a provider-independent boundary, so that another bank-data
  provider can be added without changing matching or reporting behavior. Enable Banking is the first provider.

**Notifications**

- **FR-023**: The system MUST support multiple notification channels behind a common boundary, initially
  Telegram (bot messages) and email (via a configured mail server). Each channel MUST accept a list of
  recipients (Telegram chat IDs, email addresses), and every recipient receives the same full digest.
- **FR-024**: One digest per run MUST be sent to all configured channels if and only if there is at least one
  missing transaction, consent warning or problem. A clean run MUST send nothing. There is no periodic
  "all good" message.
- **FR-025**: The digest MUST group missing transactions by account. Each line shows date, amount, currency, a
  short description, and "pending" and "last reminder" flags where applicable. Unchecked accounts appear with
  their reasons, and consent warnings are included.
- **FR-025a**: A missing transaction MUST carry a "possible match" hint when a Firefly III entry on the same
  account with the same currency and signed amount either (a) was paired with a different bank transaction
  although it was within tolerance of this one, or (b) lies outside the tolerance but within twice the
  tolerance. The hint names the nearest such entry's date and Firefly III id, so that matching ambiguity and near
  misses are reported rather than silently decided. Hints never change whether a transaction is matched or
  missing.
- **FR-026**: A transaction MUST be flagged "last reminder" when its date is the first day of the window.
- **FR-027**: A Telegram digest longer than one message MUST be split across several messages at line
  boundaries.
- **FR-028**: If delivery to one recipient or channel fails, the remaining recipients and channels MUST still be
  attempted, and each failure MUST be logged as a warning. If delivery fails for every recipient, the digest
  MUST be written to the error output and the run MUST end with the "check failed" outcome.

**Outcomes and errors**

- **FR-029**: The `check` command MUST end with exit status 0 when everything was verified and nothing is
  missing, 1 when missing transactions were found and everything was checked, and 2 when any account was
  unchecked, any consent has expired, digest delivery failed for every recipient, or the run could not complete. Status 2 takes
  precedence over 1. A consent-expiry warning alone does not change the status.
- **FR-030**: Configuration and saved state MUST be validated at startup. Any problem MUST stop the command before
  it contacts any external service, with a message naming the problem and exit status 2.
- **FR-031**: Temporary failures when contacting external services (network errors, server errors, rate limits)
  MUST be retried up to 3 times with increasing delays, honoring a wait time requested by the service of up to
  60 seconds. A longer requested wait MUST stop retrying, and the affected account is reported as unchecked
  ("rate limited").
- **FR-032**: A failure for one bank or account MUST NOT prevent checking the others. The failed ones MUST be
  reported as unchecked with a reason ("rate limited", "bank error", "consent expired", "no Firefly III account
  mapped", "ambiguous mapping", etc.).
- **FR-033**: If Firefly III cannot be reached, the system MUST send a problem-only digest and end with exit
  status 2.

**Security and privacy**

- **FR-034**: Secrets (Firefly III token, bank-provider credentials and key, notification credentials) MUST be
  supplied only through environment variables or separate secret files referenced from configuration, never
  inline in the configuration file.
- **FR-035**: Secrets MUST never appear in logs, error messages or notifications.
- **FR-036**: IBANs and account numbers MUST be masked in logs and notifications, showing only the first 4 and
  last 4 characters (e.g. `LT12…3456`). Logs MUST NOT contain raw bank or Firefly III responses.
- **FR-037**: Saved state MUST contain only the bank connection data needed to fetch transactions. No transaction
  data is persisted.

**Observability**

- **FR-038**: Each check run MUST log a structured summary: window, number of accounts checked and unchecked,
  counts of matched, missing, deduplicated and void transactions, plus any errors.
- **FR-039**: The full structured log (including the run summary) MUST be appended to a log file whose path is
  configured. Only warnings and errors MUST be written to the error output. A clean run (and a run that only
  finds missing transactions and delivers the digest) MUST produce no terminal output, so cron sends mail only
  for real problems. This does not apply to `--stdout` mode or the interactive `auth` and `accounts` commands.

### Key Entities

- **Bank connection**: an authorized link to one configured bank (name, country), with its connected accounts
  and consent expiry. It is the only persisted data.
- **Bank account**: an account visible through a bank connection. It has an IBAN or a provider identifier, a
  name and a currency, and maps to one Firefly III asset account or is excluded.
- **Account mapping**: the link between a bank account and a Firefly III asset account. It is automatic (IBAN)
  or overridden, or the account is excluded.
- **Bank transaction**: a normalized entry from the bank with account, date (per FR-005a), signed amount in account currency,
  status (booked or pending) and short description.
- **Firefly III transaction**: an entry on a mapped Firefly III asset account with date and signed amount
  relative to that account (splits summed per account).
- **Reconciliation result**: per account, the matched pairs, the missing bank transactions (each with an
  optional possible-match hint, FR-025a), or the reason the account was unchecked.
- **Digest**: the per-run message sent to notifiers, containing missing transactions, unchecked accounts and
  consent warnings.
- **Notifier**: a delivery channel for the digest (Telegram, email).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of bank transactions within the window that have no Firefly III counterpart appear in the
  digest of the next run. Zero missed transactions are allowed on a successfully completed run.
- **SC-002**: No bank transaction that has a valid Firefly III counterpart (same account, direction and exact
  amount within the tolerance) is reported missing.
- **SC-003**: Across any number of runs, the tool makes zero write changes to Firefly III and zero payment
  requests to banks.
- **SC-004**: A run in which any account could not be checked never ends with the "clean" or plain "missing
  found" outcome.
- **SC-005**: The owner receives a consent renewal warning at least 7 days (the configured period) before any
  bank consent expires, provided the job runs at least daily.
- **SC-006**: A clean run sends no notification at all.
- **SC-007**: A first-time setup (configuration, one bank authorization, verifying mappings with the accounts
  command) takes under 30 minutes for the owner.
- **SC-008**: A check run covering up to 10 accounts and 30 days finishes in under 2 minutes when each
  external request completes in under 2 seconds.
- **SC-009**: No secret or unmasked IBAN appears in any log line or notification produced by the tool.

## Assumptions

- There is a single owner/operator. The spouse's transactions reach the tool only through shared or connected
  bank accounts. There are no multi-user features.
- The tool runs on the owner's Linux server via cron. Overlapping runs are prevented by the scheduler setup
  (e.g. a lock wrapper), and run frequency respects PSD2 unattended-access limits (typically about 4 data
  fetches per account per day, so every 6 hours at most). Both points are documented, not enforced.
- Log file rotation is handled by the host (e.g. logrotate). The tool only appends.
- Firefly III is self-hosted, reachable from the server, and the owner has a personal access token. The token is
  not scoped, which is why read-only behavior is enforced by the tool itself (FR-001).
- Enable Banking is the initial bank-data provider. The owner has registered an application with it and
  whitelisted a redirect address. The redirect address does not need to host anything, because the owner copies
  it from the browser.
- How far back transactions are available depends on each bank. The default 30-day window is within the typical
  limits.
- Firefly III asset accounts have IBANs and currencies filled in where available. Accounts without them are mapped via
  configuration.
- Bank and Firefly III amounts for an account are in that account's currency. Foreign-currency details are not
  used for matching.
- The configuration file is human-edited and holds non-secret settings. Secrets live in env vars or files.
- Out of scope for v1: creating or importing transactions, ignore rules or dismiss actions, reminder history,
  a web interface, a built-in scheduler, heartbeat messages, and notification channels other than Telegram and
  email.
