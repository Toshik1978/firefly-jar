# Contract: Reminder Digest

One digest per run, identical for every recipient (FR-023–FR-027). Plain text (no markup), UTF-8.
Sent only if there is ≥1 missing transaction, consent warning, or problem (FR-024).

## Layout

Sections appear in this order and are omitted when empty.

```text
firefly-jar: 3 missing, 1 unchecked account (window 2026-08-24 – 2026-09-22)

⚠ Problems
- swedbank LT12…3456 (Main): unchecked — rate limited
- revolut LT99…0001 USD: unchecked — ambiguous mapping (Firefly #21, #22)

⏰ Consent
- seb: consent expires 2026-09-27 (5 days) — run: firefly-jar auth seb

Missing in Firefly III
Swedbank · LT12…3456 · Main (EUR)
- 2026-09-21  -4.50 EUR  COFFEE ISLAND VILNIUS  ⏳ pending
- 2026-09-19  -63.12 EUR  MAXIMA LT
- 2026-09-15  -4.50 EUR  COFFEE ISLAND VILNIUS  ≈ Firefly #7 on 2026-09-13 (paired with another)
Revolut · LT99…0001 · Revolut EUR (EUR)
- 2026-08-24  -12.00 EUR  NETFLIX.COM  🔚 last reminder
```

## Rules

- Header: counts of missing transactions and unchecked accounts, plus the window (inclusive dates).
- Account heading: `<bank display name> · <masked IBAN or hash:xxxx…xxxx> · <bank account name> (<CUR>)`.
- Line: `<date YYYY-MM-DD>  <signed amount, account minor-unit precision, '.' decimal> <CUR>  <description>`
  followed by flags `⏳ pending`, `🔚 last reminder` where applicable, then an optional hint (FR-025a):
  `≈ Firefly #<id> on <YYYY-MM-DD> (paired with another)` for `Taken`, or
  `≈ Firefly #<id> on <YYYY-MM-DD> (N days apart)` for `NearMiss`.
- Lines within an account sorted by date descending, then amount, then description (deterministic).
- Accounts sorted by bank key, then masked identifier, then currency.
- Description: counterparty name, else first remittance line, else `(no description)`; control characters
  removed; truncated to 60 characters with `…`.
- IBANs masked as first 4 + `…` + last 4 (FR-036). No secrets, no full identifiers.

## Telegram splitting

- Limit per message: 4096 characters as counted by Telegram (UTF-16 code units; see research.md).
- Split only at line boundaries. Each continuation message repeats the header line with a `(2/3)` style
  suffix. No line is lost or duplicated (FR-027).
- A single line longer than the limit cannot occur (descriptions are truncated).

## Email

- Subject: header line (first line of the digest).
- Body: full digest, `text/plain; charset=utf-8`. Never split.
