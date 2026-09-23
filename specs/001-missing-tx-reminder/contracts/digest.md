# Contract: Reminder Digest

One digest per run, identical for every recipient (FR-023–FR-027). Plain text (no markup), UTF-8.
Sent only if there is ≥1 missing transaction, consent warning, or problem (FR-024).

## Layout

Sections appear in this order and are omitted when empty.

```text
firefly-jar: 4 missing, 2 unchecked accounts (window 2026-08-24 – 2026-09-22)

⚠ Problems
- revolut LT99…0001 (Revolut USD): unchecked — ambiguous mapping (Firefly #21, #22)
- swedbank LT12…7890 (Savings): unchecked — rate limited

⏰ Consent
- seb: consent expires 2026-09-27 (5 days) — run: firefly-jar auth seb

Missing in Firefly III
Revolut · LT99…0001 · Revolut EUR (EUR) → Revolut (€)
- 2026-08-24  -12.00 EUR  NETFLIX.COM  🔚 last reminder
Swedbank · LT12…3456 · Main (EUR) → Checking - Swedbank
- 2026-09-21  -4.50 EUR  COFFEE ISLAND VILNIUS  ⏳ pending
- 2026-09-19  -63.12 EUR  MAXIMA LT
- 2026-09-15  -4.50 EUR  COFFEE ISLAND VILNIUS  ≈ Firefly #7 on 2026-09-13 (paired with another)
```

## Rules

- Header: counts of missing transactions and unchecked accounts, plus the window (inclusive dates).
- Problems: run-level problems first, `- <scope>: <reason>`, sorted by scope then reason. A Firefly III
  accounts listing that fails reads `- firefly: Firefly III unauthorized: the API token was rejected — check
  firefly.token_file or FIREFLY_JAR_FIREFLY_TOKEN` for a 401 or 403, and `- firefly: Firefly III unreachable:
  <error>` for anything else. Then one line per unchecked account in account order, `- <bank key> <masked
  IBAN or hash:xxxx…xxxx> (<bank account name>): unchecked — <reason>`, followed by `: <detail>` when there
  is one and, for an ambiguous mapping, the candidate ids ` (Firefly #21, #22)`.
- Account heading: `<bank display name> · <masked IBAN or hash:xxxx…xxxx> · <bank account name> (<CUR>) →
  <Firefly III account name>`, so the heading says where the transactions come from and where they are
  entered. Both names are checked after sanitizing (control and format characters removed, same
  convention as the description and unchecked detail); when nothing is left, or only whitespace, that
  segment (` · <bank account name>` or ` → <Firefly III account name>`) is omitted rather than left blank,
  e.g. `<bank display name> · <masked IBAN or hash:xxxx…xxxx> (<CUR>) → <Firefly III account name>`.
- Line: `<date YYYY-MM-DD>  <signed amount, account minor-unit precision, '.' decimal> <CUR>  <description>`
  followed by flags `⏳ pending`, `🔚 last reminder` where applicable, then an optional hint (FR-025a):
  `≈ Firefly #<id> on <YYYY-MM-DD> (paired with another)` for `Taken`, or
  `≈ Firefly #<id> on <YYYY-MM-DD> (N days apart)` for `NearMiss`.
- Lines within an account sorted by date descending, then amount, then description (deterministic).
- Accounts sorted by bank key, then masked identifier, then currency.
- Description: counterparty name, else first remittance line, else `(no description)`; control characters
  removed; truncated to 60 characters with `…`.
- IBANs masked as first 4 + `…` + last 4 (FR-036), including an IBAN inside free text, compact or written
  in 4-character groups (`LT12 3456 7890 1234 5678` → `LT12…5678`). No secrets, no full identifiers.

## Telegram splitting

- Limit per message: 4096 characters as counted by Telegram (UTF-16 code units; see research.md).
- Split only at line boundaries. Each continuation message repeats the header line with a `(2/3)` style
  suffix. No line is lost or duplicated (FR-027).
- A single line longer than the limit cannot occur (descriptions are truncated).

## Email

- Subject: header line (first line of the digest).
- Body: full digest, `text/plain; charset=utf-8`. Never split.
