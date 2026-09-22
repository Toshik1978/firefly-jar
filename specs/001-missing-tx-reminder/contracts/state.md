# Contract: State File

Path from `state_file`. JSON, mode `0600`, written atomically (research R11). It contains only bank
connection data and never any transactions (FR-037). It is written only by `auth`. `check` and `accounts`
read it and never modify it.

```json
{
  "version": 1,
  "sessions": {
    "swedbank": {
      "provider": "enablebanking",
      "session_id": "5a1c…",
      "valid_until": "2027-03-21T10:15:00Z",
      "authorized_at": "2026-09-22T10:15:00Z",
      "accounts": [
        {
          "uid": "07cc67f4-45d6-494b-adac-09b5cbc7e2b5",
          "hash": "WwpbCiJhY2NvdW50IiwKImFjY291bnRfaWQiLAoiaWJhbiIKXQpd.E8Gz…",
          "iban": "LT121000011101001000",
          "currency": "EUR",
          "name": "Main"
        }
      ]
    }
  }
}
```

## Rules

- An unknown `version` means the file is invalid and the command exits 2 (FR-030).
- A session for a bank key that is no longer in config is ignored, with a WARN.
- A configured bank with no session is `NOT_AUTHORIZED` (data-model.md).
- `auth <bank>` replaces only that bank's entry. Other entries are preserved byte-for-byte in meaning.
- The file is missing on first run. `auth` creates it, and `check` treats a missing file as having no sessions.
