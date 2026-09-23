# firefly-jar

`firefly-jar` is a one-shot command-line tool that compares recent bank transactions with a
self-hosted [Firefly III](https://www.firefly-iii.org/) instance and sends a reminder digest
(Telegram and/or email) listing the bank transactions nobody entered yet.

**It is a reminder, not an importer.** It never creates, edits or deletes anything in Firefly III,
and it never touches the bank beyond reading transactions:

- Against Firefly III it only ever issues `GET` requests. This is enforced in code, not just by
  convention: the HTTP transport rejects any other method before the request leaves the process.
- At the bank it requests read-only *account information* consent only. It never requests payment
  access and never sends the headers that would claim a human is present at the keyboard.

You still enter every transaction into Firefly III by hand. `firefly-jar` just tells you which ones
you forgot.

It runs once per invocation and exits — there is no daemon, no scheduler and no HTTP server. Cron
(or any other scheduler) decides when it runs.

## Requirements

- A Linux server with `cron` and `flock` (for scheduling).
- A reachable, self-hosted Firefly III instance and a personal access token
  (Options → Profile → OAuth).
- A bank connected through [Enable Banking](https://enablebanking.com/), the only bank-data provider
  supported today. You need an Enable Banking application (with its private key) and a whitelisted
  redirect URL.
- A Telegram bot token and chat id, and/or SMTP credentials, for delivering the digest.
- Go 1.27.1, only if you are building from source.

## Install

Build a static binary with the bundled task runner:

```sh
task build
```

This produces `./firefly-jar` (`CGO_ENABLED=0`, stripped, with paths trimmed from the binary). Copy
it wherever you run cron jobs from, for example:

```sh
sudo install -m 0755 firefly-jar /usr/local/bin/
```

## Configure

Configuration is a single strict YAML file — an unknown key is a load error, not a warning. Start
from [`config.example.yaml`](config.example.yaml) at the repository root, which documents every key
with placeholder values (`example.com` hosts, a zeroed UUID, masked-style IBANs). Copy it to your
real config path and fill in your own values:

```sh
# <user> is the account cron runs firefly-jar as
sudo install -d -m 0700 -o <user> /etc/firefly-jar /var/lib/firefly-jar /var/log/firefly-jar
sudo cp config.example.yaml /etc/firefly-jar/config.yaml
# edit /etc/firefly-jar/config.yaml
```

The full field-by-field reference lives in
[`specs/001-missing-tx-reminder/contracts/config.md`](specs/001-missing-tx-reminder/contracts/config.md);
this section only covers the parts worth calling out.

Every command reads `--config <path>`, defaulting to `/etc/firefly-jar/config.yaml`, or the
`FIREFLY_JAR_CONFIG` environment variable if `--config` is not given.

### Secrets

Secrets never go inline in the config file. Each one comes from either an environment variable or a
`*_file` path named in the config — a non-empty environment variable wins if both are set:

| Secret | Environment variable | Config key |
|---|---|---|
| Firefly III personal access token | `FIREFLY_JAR_FIREFLY_TOKEN` | `firefly.token_file` |
| Enable Banking private key (PEM text) | `FIREFLY_JAR_ENABLEBANKING_PRIVATE_KEY` | `enablebanking.private_key_file` |
| Telegram bot token | `FIREFLY_JAR_TELEGRAM_TOKEN` | `notify.telegram.bot_token_file` |
| SMTP password | `FIREFLY_JAR_SMTP_PASSWORD` | `notify.email.password_file` |

Only the secrets a given command actually needs are read; for example `accounts` never touches the
Telegram or SMTP secrets.

Keep secret files readable only by the account that runs `firefly-jar`:

```sh
chmod 600 /etc/firefly-jar/firefly.token
```

A secret file with group or other permission bits still works, but `firefly-jar` logs a warning
naming the file every time it reads it.

## Walkthroughs

### Connect a bank: `auth <bank>`

`<bank>` is one of the keys under `banks:` in your config. Run it once per bank, interactively:

```text
$ firefly-jar auth swedbank
Open this link and log in to Swedbank (LT):
  https://auth.enablebanking.com/ais/start?sessionid=…
After login your browser is redirected. Paste the full address from the address bar:
> https://example.com/eb-callback?code=…&state=…
Connected Swedbank (LT): 2 accounts, consent valid until 2027-03-21.
```

Open the printed link, log in at the bank, and your browser is redirected to your configured
`redirect_url`. That URL does not need to host anything — you just copy the full resulting address
out of the browser's address bar and paste it back into the terminal.

`firefly-jar` requests the longest consent validity the bank allows (minus a one-hour safety
margin), verifies that the pasted URL carries the anti-forgery `state` value it issued, and only
then saves the connection to the state file (mode `0600`, readable and writable only by the owning
system user). If the pasted URL is wrong, missing its `state`, or carries an `error`, nothing is
saved and the command exits 2.

Re-running `auth` for a bank you already connected replaces that bank's saved session — but only
after the new session is obtained, so a failed re-authorization never destroys a working one.

### Verify account coverage: `accounts --ids`

```text
$ firefly-jar accounts --ids
BANK      ACCOUNT        NAME            CUR  STATUS       FIREFLY
swedbank  LT12…3456      Main            EUR  auto         #12 Household EUR
swedbank  hash:Wwpb…Lqc=  Card            EUR  override     #43 Credit card
revolut   LT99…0001      Revolut EUR     EUR  auto         #20 Revolut EUR
revolut   LT99…0001      Revolut USD     USD  ambiguous    #21, #22
revolut   LT99…0001      Revolut GBP     GBP  unmapped     —
old       LT55…7777      Closed          EUR  excluded     —
```

This is a read-only report: it reads the bank accounts already saved by `auth` and the asset
accounts from Firefly III, and makes no bank call, so it costs no bank quota. Every row is one bank
account:

- **auto** — mapped automatically because its IBAN (spaces and case ignored) and currency match
  exactly one Firefly III asset account.
- **override** — mapped by an `accounts:` entry in config.
- **ambiguous** — more than one Firefly III account shares that IBAN and currency; not checked until
  you add an override.
- **unmapped** — no IBAN match and no override; not checked until you either map it or exclude it.
- **excluded** — deliberately not checked, by an `accounts:` override with `exclude: true`.

For an account without an IBAN (a card, typically), `--ids` prints its full `identification_hash` in
an extra column so you can copy it straight into an `accounts:` override, for example:

```yaml
accounts:
  - bank: swedbank
    hash: "WwpbCiJhY2NvdW50Ii…"    # copied from the ACCOUNT column above, --ids on
    firefly_account_id: 43
```

An IBAN-based override looks the same but keys on `iban` (and, for a multi-currency account under one
IBAN, `currency`) instead of `hash`. See `config.example.yaml` for both forms side by side.

`accounts` exits 0 once every non-excluded account is mapped, and 2 otherwise — run it again after
adding overrides until it reports 0. It is meant to be used exactly like a setup check.

### Look at the digest before enabling notifications: `check --stdout`

Once every account is mapped, take a dry look at what a real run would report before wiring up
Telegram or email:

```sh
firefly-jar check --stdout; echo "exit=$?"
```

`--stdout` prints the digest (if there is one) to standard output instead of sending it anywhere;
exit codes are unchanged. If everything in the window is already entered in Firefly III, nothing is
printed and the exit code is 0. Once you are happy with what it reports, drop `--stdout` and let
`check` deliver the digest to your configured notifiers instead.

## Running on a schedule

`firefly-jar` has no built-in scheduler; cron does the scheduling and `flock` prevents overlapping
runs:

```cron
MAILTO=me@example.com
0 */6 * * * flock -n /var/lib/firefly-jar/run.lock /usr/local/bin/firefly-jar check
```

Two reasons for the pieces of that line:

- **Every 6 hours, not more often.** Unattended (PSD2) access to a bank account is typically capped
  at about 4 data fetches per account per day. Running more often than every 6 hours risks running
  out of quota partway through the day.
- **`flock -n`.** If one run is still in flight (a slow bank, a retry backoff) when the next cron
  tick fires, `flock -n` skips the new run instead of letting two runs fetch from the same bank
  session at once.

A run that exits 0 or 1 writes nothing to stdout, and nothing to stderr except WARN records, even
when it found missing transactions or a consent warning: those reach you as the digest, through the
configured notifiers (Telegram, email). Only WARN and ERROR records reach stderr, such as a delivery
that failed for one recipient (exit 1) or the one-line summary of a run that exits 2, so cron mails
you only when the tool itself needs attention. If every delivery fails, the digest text itself goes to stderr as a fallback, and cron
mails that.

## Logs and log rotation

The full structured log (JSON, one line per event, including one run-summary line per run) is
appended to the `log_file` configured path. Only warnings and errors are also written to stderr,
which is what cron mails you.

Rotate the log file with `logrotate` the ordinary way:

```
/var/log/firefly-jar/firefly-jar.log {
    weekly
    rotate 8
    compress
    missingok
    notifempty
}
```

`copytruncate` is not needed and should not be used: `firefly-jar` is a one-shot process that opens
the log file fresh on every run, so a plain rename-and-recreate rotation (logrotate's default) never
loses or duplicates a line the way `copytruncate` can with a long-running process.

## Exit codes

`check`:

| Situation | Exit |
|---|---|
| Everything checked, nothing missing, no warnings | 0 |
| Everything checked, nothing missing, consent warning(s) only | 0 |
| Everything checked, ≥1 missing transaction, digest delivered | 1 |
| Any account unchecked, any consent expired, Firefly III unreachable or its token rejected, or delivery failed for every recipient | 2 |
| Configuration or saved state invalid | 2 |

Precedence is 2 > 1 > 0: a single unchecked account turns an otherwise-clean or otherwise-missing-only
run into exit 2. A consent warning alone (nothing expired yet) does not raise the exit code by
itself.

`auth <bank>`:

| Situation | Exit |
|---|---|
| Session created and saved | 0 |
| Pasted URL has a wrong or missing `state`, or carries `error=` | 2 (nothing saved) |
| Provider or network error | 2 (nothing saved) |

`accounts [--ids]`:

| Situation | Exit |
|---|---|
| Every non-excluded account is mapped | 0 |
| Any account is unmapped or ambiguous | 2 |

`--version` and `--help`/`-h` always exit 0. An unknown command, flag, or wrong number of arguments
prints usage on stderr and exits 2.

## Troubleshooting

### Consent expiry

Bank consent expires after a bank-defined period (`auth` requests the longest one the bank allows).
Before it expires, `check` warns you in the digest even if nothing is missing:

```text
⏰ Consent
- swedbank: consent expires 2026-09-27 (5 days) — run: firefly-jar auth swedbank
```

The warning appears once the expiry falls within `consent_warn_days` (default 7). Run
`firefly-jar auth <bank>` again — the same command you used the first time — to renew it; the new
session replaces the old one only after it is successfully obtained.

If you miss the warning and the consent actually expires, that bank's accounts stop being checked.
The digest reports each of them as unchecked:

```text
⚠ Problems
- swedbank LT12…3456 (Main): unchecked — consent expired
```

Other banks keep being checked normally; only the expired bank's accounts are affected. Run
`firefly-jar auth <bank>` to fix it — the reported accounts return to being checked on the next run.

### "Ambiguous mapping"

This means more than one Firefly III asset account shares the same IBAN (ignoring spaces and case)
and currency as a bank account, so automatic mapping cannot pick one:

```text
⚠ Problems
- revolut LT99…0001 (Revolut USD): unchecked — ambiguous mapping (Firefly #21, #22)
```

`firefly-jar accounts --ids` shows the same account as `ambiguous`, with the candidate Firefly III
account ids in its `FIREFLY` column. Resolve it with an `accounts:` override in config that names the
Firefly III account explicitly:

```yaml
accounts:
  - bank: revolut
    iban: LT99 0000 0000 0000 0001
    currency: USD
    firefly_account_id: 21
```

Overrides always take precedence over automatic IBAN matching, so once the override is in place the
account is checked again on the next run.

## What's out of scope

`firefly-jar` never creates or imports transactions, has no ignore/dismiss mechanism (a missing
transaction is reported on every run until it is entered or ages out of the window), keeps no
reminder history, has no web interface, and supports no notification channels beyond Telegram and
email. See
[`specs/001-missing-tx-reminder/spec.md`](specs/001-missing-tx-reminder/spec.md) for the full
functional specification.
