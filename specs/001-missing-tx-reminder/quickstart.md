# Quickstart & Validation Guide: Missing Transaction Reminder

This guide shows the feature working end to end. The formats are defined in [contracts/](contracts/), so they
are not repeated here.

## Prerequisites

- Go 1.27.1 (per `go.mod`) on the build machine, and a Linux server with cron and `flock` for deployment.
- A Firefly III instance reachable from the server, with a Personal Access Token (Options → Profile → OAuth).
- An Enable Banking application in restricted production, with your own accounts linked ("Activate by linking
  accounts"). Keep the downloaded `<app_id>.pem`. Register a `redirect_url`: any https URL you control. It
  never has to serve anything, because you copy it from the browser's address bar.
- A Telegram bot token (from @BotFather) and your chat id, and/or SMTP credentials.

## 1. Automated checks (no network)

```sh
task setup                # pinned toolchain via mise, module download
task check                # format:check → lint → test (go test -race); expect exit 0, no live calls
```

These tests must include:
- a read-only guard test: POST/PUT/PATCH/DELETE through the Firefly transport fail and never reach the test
  server (FR-001);
- reconcile table tests, including the FR-011 invariant;
- the exit code matrix in [contracts/cli.md](contracts/cli.md).

Optional live smoke test against your own instances (excluded by default):

```sh
FIREFLY_JAR_CONFIG=./config.yaml go test -tags live ./internal/app/ -run Live
```

## 2. Install and configure

```sh
task build                # static binary ./firefly-jar
sudo install -m 0755 firefly-jar /usr/local/bin/
sudo install -d -m 0700 -o finance /etc/firefly-jar /var/lib/firefly-jar /var/log/firefly-jar
# write /etc/firefly-jar/config.yaml per contracts/config.md; secrets in separate 0600 files
```

## 3. Connect a bank (User Story 2)

```sh
sudo -u finance firefly-jar auth swedbank
```

Expected: a login link is printed. After you paste the redirect URL, you see
`Connected Swedbank (LT): N accounts, consent valid until …`. `state.json` is created with mode `0600`.
Pasting a URL with a modified `state=` value gives exit 2 and leaves the state file unchanged.

## 4. Verify coverage (User Story 3)

```sh
sudo -u finance firefly-jar accounts --ids
```

Expected: every bank account shows `auto`, `override` or `excluded`. For any `unmapped` or `ambiguous` row,
add an `accounts:` override (by `iban`+`currency` or `hash`) and rerun until the exit code is 0.

## 5. First check, dry run (User Story 1)

```sh
sudo -u finance firefly-jar check --stdout; echo "exit=$?"
```

Expected: the digest is printed (or nothing, if everything is entered), with exit 0 or 1. Check it against
your Firefly III data:
- Leave one recent bank transaction unentered. It appears under its account.
- Enter it in Firefly III (same amount, date within ±3 days) and rerun. It disappears.

## 6. Real notifications

```sh
sudo -u finance firefly-jar check; echo "exit=$?"
```

Expected: a Telegram message and/or email when something is missing, and nothing on the terminal. Look at
`/var/log/firefly-jar/firefly-jar.log` for one JSON summary line per run, and check that it contains no full
IBAN or token. A non-zero `deduplicated` count shows that the bank links pending and booked entries by
`entry_reference` (research R6). If it stays at 0 while pending purchases are showing, the bank doesn't, so
expect an occasional extra reminder until the pending copy disappears.

## 7. Schedule

```cron
MAILTO=me@example.com
0 */6 * * * flock -n /var/lib/firefly-jar/run.lock /usr/local/bin/firefly-jar check
```

Every 6 hours stays within PSD2 unattended limits (research R7). Because only warnings and errors reach the
terminal (FR-039), cron mails you only for real problems.

## 8. Failure drills (User Story 4)

| Drill | Expected |
|---|---|
| Point `firefly.url` at a closed port | Problem-only digest ("Firefly III unreachable: …"), exit 2 |
| Set a wrong Firefly III token | Problem-only digest ("Firefly III unauthorized: the API token was rejected — …"), exit 2 |
| Set a wrong Telegram token, keep email valid | Email delivered, WARN on stderr, exit follows the findings |
| Break all notifiers | Digest text on stderr, exit 2 |
| Remove a bank's session from `state.json` | "not authorized — run: firefly-jar auth <bank>", exit 2 |
| Edit `valid_until` to 5 days from now | Consent warning in the digest, exit unchanged |
| Add an unknown key to config | Error naming the key before any network call, exit 2 |
