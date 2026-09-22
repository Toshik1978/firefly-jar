# Contract: Configuration File

YAML, decoded strictly (unknown keys are an error). Secrets are never inline: every secret is a `*_file`
path or read from the environment variable named in the table (env wins over file) (FR-034).

```yaml
timezone: Europe/Vilnius          # IANA name; required
window_days: 30                   # 1..365, default 30
date_tolerance_days: 3            # 0..14, default 3
consent_warn_days: 7              # 0..90, default 7
state_file: /var/lib/firefly-jar/state.json   # required; parent dir must exist
log_file: /var/log/firefly-jar/firefly-jar.log # required for `check`; appended (FR-039)
log_level: info                   # debug|info|warn|error, default info (file only)

firefly:
  url: https://firefly.example.com          # required; https (http only for localhost/private LAN hosts);
                                            # "…/api" or "…/api/v1" suffixes accepted and normalized
  token_file: /etc/firefly-jar/firefly.token  # or env FIREFLY_JAR_FIREFLY_TOKEN

enablebanking:
  app_id: 00000000-0000-0000-0000-000000000000  # required (JWT kid)
  private_key_file: /etc/firefly-jar/enablebanking.pem  # or env FIREFLY_JAR_ENABLEBANKING_PRIVATE_KEY (PEM text)
  redirect_url: https://example.com/eb-callback           # must be whitelisted in the Enable Banking app
  psu_type: personal                                      # personal|business, default personal

banks:                             # key = name used by `auth <bank>`; at least one
  swedbank: { name: Swedbank, country: LT }
  revolut:  { name: Revolut, country: LT, display: Revolut }   # display optional

accounts:                          # optional overrides (FR-015); first matching rule wins
  - bank: swedbank
    hash: "WwpbCiJhY2NvdW50Ii…"     # Enable Banking identification_hash (stable across sessions);
                                   # copy from `firefly-jar accounts --ids` (for accounts without IBAN)
    firefly_account_id: 43
  - bank: revolut
    iban: LT99 0000 0000 0000 0001
    currency: USD                  # optional; narrows an IBAN rule to one currency
    firefly_account_id: 21
  - bank: old
    iban: LT55...
    exclude: true

notify:                            # at least one channel with at least one recipient
  telegram:
    bot_token_file: /etc/firefly-jar/telegram.token   # or env FIREFLY_JAR_TELEGRAM_TOKEN
    chat_ids: [123456789, -1001234567890]
  email:
    host: smtp.example.com
    port: 587                      # 587 = STARTTLS required; 465 = implicit TLS; others rejected
    username: firefly-jar@example.com
    password_file: /etc/firefly-jar/smtp.password     # or env FIREFLY_JAR_SMTP_PASSWORD
    from: firefly-jar@example.com
    to: [me@example.com, spouse@example.com]
```

## Validation rules (FR-030)

- Required fields present; numeric ranges as annotated; `timezone` loads.
- Each override has exactly one of `hash` / `iban`, and exactly one of `firefly_account_id` / `exclude: true`;
  `bank` refers to a key in `banks`.
- Secrets are resolved **per command**. Only the secrets a command uses must resolve (env var non-empty or
  file readable). Unused secrets are not read at all:

  | Command | Required secrets | Other requirements |
  |---|---|---|
  | `auth <bank>` | Enable Banking private key | `state_file` directory writable |
  | `accounts` | Enable Banking private key, Firefly token | state file readable if it exists |
  | `check --stdout` | Enable Banking private key, Firefly token | `log_file` directory writable; state file readable if it exists |
  | `check` | the above + the secrets of every configured notifier (Telegram token, SMTP password) | the above + ≥1 notifier recipient |

- Secret files with group/other permissions produce a WARN, not an error.
- The private key parses as RSA PEM (PKCS#1 or PKCS#8).
- `log_file` is required and validated only for `check`. `auth` and `accounts` log WARN and above to stderr
  only.
- For `check`: state file readable if it exists (a configured bank with no session in state → reported as a problem
  "not authorized — run: firefly-jar auth <bank>", exit 2).
