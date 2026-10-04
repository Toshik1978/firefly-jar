# Data Model: Missing Transaction Reminder

These are in-memory domain types, except for **BankSession**, which is the only type that is persisted
(FR-037). The names are indicative. Packages are listed in plan.md.

## Value types (`internal/money`, `internal/civil`)

Elsewhere in this document, `Date` means `civil.Date` and `Range` means `civil.Range` (`internal/civil`).

| Type | Fields | Rules |
|---|---|---|
| `civil.Date` (`internal/civil`) | `Year int, Month time.Month, Day int` | A civil date in the configured time zone; a trimmed copy of `cloud.google.com/go/civil`'s `Date` (R19), with no dependency on `cloud.google.com/go`. Supports ordering and day arithmetic (`AddDays`, `DaysSince`, the signed day count that replaced the original hand-rolled date type's `DaysBetween`). |
| `Amount` (`internal/money`) | `Value decimal.Decimal, Currency string` | Exact decimal, backed by `github.com/shopspring/decimal` (R15), parsed with no float through a grammar check stricter than `decimal.NewFromString` alone. `Equal` compares currency and value; `decimal.Decimal`'s own normalization means `"12.340000000000"` equals `"12.34"`. More than 18 significant digits is rejected (R15). |
| `civil.Range` (`internal/civil`) | `From civil.Date, To civil.Date` (inclusive) | The check window, this repository's own code beside the upstream `Date` copy: `From = today − (window_days − 1)` and `To = today` (FR-004). `IsFirstDay(d)` means `d == From` (FR-026). |

## Bank side (`internal/bank`)

### BankAccount
| Field | Type | Notes |
|---|---|---|
| `BankKey` | string | The config key under `banks:`. |
| `UID` | string | The provider's per-session account id. Used only to call the provider. |
| `Hash` | string | The provider's stable identification (`identification_hash`). Used by overrides (R4). |
| `IBAN` | string | Normalized: uppercase with no spaces. Empty if the bank has none. |
| `Currency` | string | ISO 4217 code. |
| `Name` | string | The bank's account name. Display only. |

Display identifier: masked IBAN (`LT12…3456`) if an IBAN is present, otherwise `hash:` plus the first 4 and
last 4 characters of the hash.

### BankTransaction
| Field | Type | Notes |
|---|---|---|
| `Account` | BankAccount ref | |
| `Date` | Date | Chosen in the order transaction_date, booking_date, value_date (FR-005a). |
| `Amount` | Amount | Signed: debit negative, credit positive (R5). Zero means `Void`. |
| `Status` | enum `Booked \| Pending \| Void` | See the mapping table in R5. |
| `EntryRef` | string | May be empty. Used only for pending/booked deduplication (FR-005b, R6). |
| `Description` | string | Counterparty or remittance text, sanitized (control characters removed). Appears only in the digest, **never in logs** (constitution §V). |

### BankSession (persisted in the state file; see [contracts/state.md](contracts/state.md))
| Field | Type | Notes |
|---|---|---|
| `BankKey` | string | One session per configured bank. |
| `SessionID` | string | Secret-like: never logged in full. |
| `ValidUntil` | timestamp (RFC 3339) | Consent expiry. |
| `Accounts` | []BankAccount | Snapshot taken at authorization time. |
| `AuthorizedAt` | timestamp | Informational. |

**Consent state** is derived at each run relative to `now`:

```text
            now < ValidUntil − warn_days          →  OK
ValidUntil − warn_days ≤ now < ValidUntil         →  EXPIRING  (warning in digest, still checked)
            now ≥ ValidUntil  or provider says    →  EXPIRED / REVOKED  (all accounts unchecked)
            EXPIRED_/REVOKED_/CLOSED_SESSION
no session in state for a configured bank          →  NOT_AUTHORIZED  (run-level Problem, not an
                                                       unchecked account: the accounts are unknown; exit 2)
```

### Authorizer (provider-neutral consent flow, constitution §IV)
```text
type Authorizer interface {
    Begin(ctx, bank config.Bank) (Pending{URL string, State string}, error)
    Complete(ctx, bank config.Bank, pending Pending, pastedRedirect string) (state.Session, error)
    Revoke(ctx, sessionID string) error   // best effort
}
```
`app/auth.go` depends only on this interface. The Enable Banking adapter implements it, and the session's
`provider` field selects the implementation.

## Firefly III side (`internal/firefly`)

### FireflyAccount
| Field | Type | Notes |
|---|---|---|
| `ID` | string | Firefly III ids are strings. |
| `Name` | string | |
| `IBAN` | string | Normalized. May be empty. |
| `Currency` | string | `currency_code` |
| `Role` | string | `account_role`, e.g. `ccAsset` for credit cards. Display only. |
| `DecimalPlaces` | uint8 | Display precision for this account's currency (R15). |
| `Active` | bool | Only active accounts are auto-mapped (R8). A missing value counts as active. |

### FireflyEntry
| Field | Type | Notes |
|---|---|---|
| `GroupID` | string | The transaction group id. |
| `AccountID` | string | The asset account the entry is relative to. |
| `Date` | Date | The `YYYY-MM-DD` prefix of the first counted split's `date` as rendered by Firefly III (FR-006a, R8): the first split that counts toward `Amount`, so a leading split that is skipped (another account, an uncounted type, or not comparable in the account's currency) never sets the date. |
| `Amount` | Amount | Signed relative to the account, in the account's currency (`amount` or `foreign_amount`). Comparable splits in the group are summed (FR-009, R8). Only `withdrawal`, `deposit` and `transfer` splits count. |
| `Description` | string | Kept in memory for tests and debugging. **Never logged** (constitution §V). |

## Mapping (`internal/accountmap`)

### AccountMapping
| Field | Type | Notes |
|---|---|---|
| `Bank` | BankAccount | |
| `Status` | enum `Auto \| Override \| Excluded \| Ambiguous \| Unmapped` | |
| `Firefly` | *FireflyAccount | Set only for `Auto` and `Override`. |
| `Candidates` | []FireflyAccount | Set only for `Ambiguous`. |

**Resolution order** for each bank account (FR-014, FR-015):
1. The first override rule matching `bank` and (`hash` == Hash, or `iban` == IBAN and optionally
   `currency` == Currency). If found: `Excluded` or `Override`. An override pointing to a Firefly id that
   doesn't exist makes the account `Unmapped`, reported as "override target #N not found".
2. Otherwise, active Firefly III asset accounts with the same IBAN and Currency: exactly one → `Auto`; more than one →
   `Ambiguous`; none → `Unmapped`.
3. A bank account with no IBAN and no override → `Unmapped`.

## Reconciliation (`internal/reconcile`)

### Input and output
```text
Reconcile(bankTxs []BankTransaction, entries []FireflyEntry, tolerance int, window Range) → AccountResult
```

### AccountResult
| Field | Type |
|---|---|
| `Mapping` | AccountMapping |
| `Matched` | []Pair{Bank BankTransaction, Firefly FireflyEntry} |
| `Missing` | []MissingItem{Tx BankTransaction, Pending bool, LastReminder bool, Hint *Hint} |
| `Deduplicated` | int |
| `Void` | int |
| `Unchecked` | *UncheckedReason | Non-nil means none of the other fields apply. |

### Hint (FR-025a)
`{GroupID string, Date Date, Kind enum Taken|NearMiss}`. `Taken` means the entry was within tolerance of this
bank transaction but was paired with another one. `NearMiss` means the entry lies outside the tolerance but
within 2× the tolerance. It is informational only and never changes a match.

### UncheckedReason
`{Code enum, Detail string}` with codes: `ConsentExpired`, `ConsentRevoked`, `RateLimited`,
`BankError`, `BankDataIncomplete`, `FireflyError`, `FireflyDataIncomplete`, `Unmapped`, `Ambiguous`, `OverrideTargetMissing`.

### Algorithm (FR-007, FR-008, FR-010, FR-011)
1. Keep bank transactions with `Date` on or after `window.From` (`inWindow`), including one dated after today
   (FR-004a). Count `Void` and set those entries aside.
2. Deduplicate within `inWindow`: for each `EntryRef` present on both a Booked and a Pending entry, drop the
   Pending one and increment `Deduplicated`. A pending copy whose booked twin falls outside the window stays.
3. Group bank transactions and Firefly entries by `(Amount.Currency, Amount value)`. The account is fixed per
   call. A different currency never matches.
4. In each group, sort the bank transactions by `(Date, Currency, numeric value, EntryRef, Description,
   Status)` with Booked before Pending, and the Firefly entries by `(Date, GroupID)`, where group ids are
   compared numerically when both are integers (`"99"` before `"100"`, as in Firefly III) and as strings
   otherwise. For each bank transaction in that order, pair it with the earliest unused Firefly entry where
   `|entry.Date − tx.Date| ≤ tolerance`.
5. Unpaired bank transactions become `Missing`, with `Pending = Status == Pending` and
   `LastReminder = window.IsFirstDay(tx.Date)`.
6. Hints (FR-025a): for each Missing item, look at the Firefly entries in the same bucket. Candidates are
   (a) entries within `tolerance` that were paired with another bank transaction (`Taken`), and (b) unpaired
   or paired entries with `tolerance < |Δ| ≤ 2×tolerance` (`NearMiss`). Pick the one with the smallest `|Δ|`,
   breaking ties by earlier date and then group id (the numeric rule of step 4). A Taken candidate is always
   nearer than a NearMiss one, so the two kinds never tie. No candidates → `Hint = nil`.
7. Invariant, asserted in tests. With `inWindow` = fetched transactions dated on or after `window.From`, before
   deduplication: `len(inWindow) == len(Matched) + len(Missing) + Deduplicated + Void`. Nothing is dropped
   silently (FR-011).

## Run outcome (`internal/report`)

### RunReport
| Field | Type |
|---|---|
| `Window` | Range |
| `Accounts` | []AccountResult |
| `ConsentWarnings` | []{BankKey, ValidUntil, DaysLeft} |
| `Problems` | []{Scope (bank or firefly), Reason} — failures that are not per-account (e.g. Firefly III unreachable) |
| `Delivery` | {Attempted, Succeeded int, Failures []{Channel, Recipient masked, Reason}} |

**Exit status** (FR-029):
`2` if any account is Unchecked, any Problem exists, or `Attempted > 0 && Succeeded == 0`. Otherwise `1` if
any Missing. Otherwise `0`.

**Digest needed** (FR-024): any Missing, any ConsentWarning, any Unchecked, or any Problem.

**Summary** (FR-038, the INFO run-summary record): `accounts_checked`, `accounts_unchecked` and
`accounts_excluded` (an account set aside by an `exclude: true` rule: never fetched, never in the digest,
counted only here), then the reconcile counts `matched`, `missing`, `deduplicated` and `void` over checked
accounts only.

## Digest (`internal/digest`)
`Render(RunReport) → Digest{Subject string, Lines []string}`, following
[contracts/digest.md](contracts/digest.md). Telegram splitting is done by the Telegram notifier on `Lines`.

## Notifier (`internal/notify`)
```text
type Notifier interface { Name() string; Send(ctx, Digest) []RecipientResult }
```
Fan-out calls every notifier. Each notifier tries every recipient. Results are merged into
`RunReport.Delivery` (FR-028).
