# Architecture

This document explains the shape of `firefly-jar` and, where it matters, why it is shaped that way.
[`README.md`](../README.md) says what the finished program does, and the feature's
[spec, plan, research and contracts](../specs/001-missing-tx-reminder/) hold the full detail this
document summarizes. [`.claude/CLAUDE.md`](../.claude/CLAUDE.md) lists the rules for changing the
code.

## One run, one process

`firefly-jar` is a one-shot command. Cron starts it, it does one reconciliation, and it exits with
a code cron can act on. There is no daemon, no scheduler and no HTTP server, so there is no
long-lived state in memory and nothing to supervise. Everything that must survive between runs is
in one small state file: the authorized bank sessions and the accounts each one exposes. It never
holds a transaction.

Three commands share the binary:

| Command | Does | Writes |
| --- | --- | --- |
| `check` | fetches, reconciles, sends the digest | the log file only |
| `auth <bank>` | starts or renews a bank's read-only consent | the state file |
| `accounts` | shows how every bank account maps to Firefly III | nothing |

Only `auth` writes the state file, so a `check` can never lose a consent by crashing half-way.

## Layering

```
cmd/firefly-jar                 the entry point: one call into internal/app
internal/app                    commands, wiring, the check pipeline, the exit code
  ├── config                    strict YAML, defaults, validation, per-command secrets
  ├── state                     the authorized banks and the accounts each session saw
  ├── logging                   JSON log file + WARN-and-above on stderr, one redactor
  ├── bank                      the provider-neutral Provider and Authorizer interfaces
  │     └── enablebanking       the Enable Banking adapter (JWT, sessions, transactions)
  ├── firefly                   the read-only Firefly III client
  ├── accountmap                bank account → Firefly III account (overrides, then IBAN)
  ├── reconcile                 the pure matching core
  ├── report                    the run outcome: exit code, whether a digest is needed
  ├── digest                    the plain-text digest, a pure renderer
  └── notify                    fan-out and delivery bookkeeping
        ├── telegram            Bot API sendMessage
        └── email               SMTP with STARTTLS or implicit TLS
shared leaves: money, civil, httpclient, redact
```

`internal/app` is the only package that knows every other one. The adapters (`enablebanking`,
`firefly`, `telegram`, `email`) never import each other. The core (`accountmap`, `reconcile`,
`report`, `digest`) uses the value types of `bank` and `firefly` (an account, a transaction) but
never their clients or transports. That split is what lets the core be tested with constructed
values alone: a change to matching is proven with a table of bank and Firefly III transactions, with
no HTTP involved.

The shared leaves import nothing from this repository:

- `money` is an exact decimal amount in one currency (`shopspring/decimal`). Money is never a
  float anywhere.
- `civil` is a calendar date with no time zone (`date.go`, a trimmed copy of
  `cloud.google.com/go/civil`) plus `Range`, the check window.
- `httpclient` is the outbound HTTP stack: per-attempt timeouts, a retry loop on
  `avast/retry-go` for idempotent requests, and no automatic redirects.
- `redact` masks secrets and IBANs before any string reaches a log, an error or a digest.

## The check pipeline

`app.Check` runs these steps in order, with one clock reading taken at the start:

1. **Today and the window.** "Today" is computed once in the configured time zone and the window
   (`civil.Range`) is derived from it, so every account and every consent in the run is judged
   against the same day.
2. **Firefly III accounts.** Listed before any bank call. If Firefly III is unreachable or rejects
   the token, no bank account could be mapped anyway, so the run stops here without spending bank
   API quota and reports the problem.
3. **Per bank.** For each configured bank: an expired consent marks its accounts unchecked without
   calling the bank. Otherwise `accountmap` maps the session's accounts to Firefly III accounts,
   and for each mapped account the bank's transactions and the Firefly III entries in the window
   (widened by the date tolerance) are fetched and reconciled. An unmapped or ambiguous account is
   reported unchecked, and one bank's failure never stops another bank from being checked.
4. **Report.** `report.RunReport` gathers every account's result and every run-level problem. It
   decides the exit code and whether a digest is needed at all.
5. **Digest and delivery.** `digest.Render` turns the report into text; `notify` sends it to every
   configured Telegram chat and email address. If every recipient fails, the full digest goes to
   stderr so cron mails it anyway.
6. **Summary.** One INFO record in the log file carries the run's counts.

## Reconciliation never drops a transaction

`reconcile.Reconcile` compares one account's bank transactions with its Firefly III entries. Every
bank transaction dated in the window ends up in exactly one bucket: matched, missing, deduplicated
(a pending copy of a transaction the bank also reports as booked) or void (cancelled, rejected,
scheduled, or a zero amount such as a card check). The tests assert
`inWindow == matched + missing + deduplicated + void` for every scenario, so a transaction cannot
vanish without failing the build.

A Firefly III transaction group counts as one entry: its splits on the account are summed, so one
bank payment entered as several splits still matches. Matching is by exact amount and currency
within a ± date tolerance. Both sides are sorted into a fixed order and each bank transaction takes
the earliest unused entry in range, so the same inputs always give the same result whatever order
they arrive in. A missing transaction that is close to an entry carries a hint, such as
`≈ Firefly #123 on 2026-01-02 (2 days apart)`, so the owner can tell a mistyped date from a
genuinely missing entry.

`reconcile` does no I/O, logs nothing and never modifies its inputs. The data model is in
[`data-model.md`](../specs/001-missing-tx-reminder/data-model.md).

## Exit codes are the interface to cron

| Exit | Meaning |
| --- | --- |
| 0 | every checked account is fully entered |
| 1 | at least one transaction is missing; the digest says which |
| 2 | the run is incomplete: an unchecked account, a run-level problem, or every delivery failed |

A run that exits 0 or 1 writes nothing to stdout or stderr, because cron mails any output: the
digest is the notification, and a clean run should be silent. Only WARN and above reach stderr.
The rules are in [`contracts/cli.md`](../specs/001-missing-tx-reminder/contracts/cli.md).

## Read-only is enforced, not promised

Two guarantees are structural rather than a matter of discipline:

- **Firefly III.** The client exposes list methods only, and its transport
  (`firefly.ReadOnlyTransport`) rejects every method except `GET` before the request leaves the
  process. A test sends every other method, and none reaches the server.
- **The bank.** The Enable Banking adapter asks for account-information consent only, never calls
  a payment endpoint, and never sends a `Psu-*` header. Those headers tell a bank that a person is
  present, and an unattended cron job must not claim that.

## Secrets and personal data

Secrets come only from environment variables or `*_file` paths, and `config.ValidateFor` reads
only the ones the command being run needs. They are kept out of the YAML structs and never reach a
log, an error or a digest. Every log record passes through one redacting `ReplaceAttr`, which masks
IBANs (`LT12…3456`) and secrets the same way everywhere. Transaction descriptions and counterparty
names never appear in logs at any level; they appear only in the digest, which goes to the owner.

## Provider-neutral bank integration

`bank.Provider` (fetch transactions) and `bank.Authorizer` (start and finish a consent) are the only
things `app` knows about a bank. Enable Banking is the one implementation today. Another provider
would be a new adapter package behind the same two interfaces, with no change to `reconcile`,
`digest` or `notify`.

## Testing

Every package is tested with testify suites and the race detector. HTTP adapters use per-client
`httpmock` transports and anonymized fixtures in `testdata/`. Retry and backoff timing runs under
`testing/synctest`, so no test sleeps. The digest is covered by golden files that are reviewed like
code. Live smoke tests exist only behind the `live` build tag and never run in `task check`. CI
publishes coverage and gates it at the floor in `Taskfile.yml`.
