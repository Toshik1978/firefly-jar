# Contract: Command-Line Interface

Binary: `firefly-jar`. All commands take `--config <path>` (default `/etc/firefly-jar/config.yaml`,
env override `FIREFLY_JAR_CONFIG`). Unknown flags or commands → usage on stderr, exit 2.

Config and state are validated before any network call (FR-030); on failure: one-line error on stderr naming
the field/file, exit 2.

## `firefly-jar check [--stdout]`

One reconciliation run (FR-003).

| Situation | Notifications | stdout | stderr | Exit |
|---|---|---|---|---|
| All accounts checked, nothing missing, no warnings | none | — | — | 0 |
| All accounts checked, nothing missing, consent warning(s) | digest (warnings only) | — | — | 0 |
| All accounts checked, ≥1 missing, digest delivered to ≥1 recipient | digest | — | per-recipient failures (WARN) | 1 |
| Any account unchecked / consent expired / Firefly III unreachable | digest (with problems) | — | ERROR summary line | 2 |
| Delivery failed for every recipient | — | — | ERROR + full digest text | 2 |
| Config/state invalid | — | — | ERROR | 2 |

- `--stdout`: the digest (if any) is printed to stdout instead of being sent; exit codes unchanged.
  Delivery-failure rows do not apply.
- A non-`--stdout` run that exits 0 or 1 writes nothing to stdout/stderr (FR-039).
- Precedence: 2 > 1 > 0 (FR-029).

## `firefly-jar auth <bank>`

Interactive; `<bank>` is a key under `banks:` in config (unknown key → exit 2).

```text
$ firefly-jar auth swedbank
Open this link and log in to Swedbank (LT):
  https://auth.enablebanking.com/ais/start?sessionid=…
After login your browser is redirected. Paste the full address from the address bar:
> https://example.com/eb-callback?code=…&state=…
Connected Swedbank (LT): 2 accounts, consent valid until 2027-03-21.
```

| Situation | Exit |
|---|---|
| Session created and state saved | 0 |
| Pasted URL has wrong/missing `state`, or carries `error=` | 2, nothing saved |
| Provider/network error | 2, nothing saved |

Existing session for the same bank is replaced only after the new one is obtained (FR-018, FR-019).

## `firefly-jar accounts [--ids]`

Read-only mapping report (FR-017). Reads bank accounts from the state snapshot and makes **no bank-provider
call**, so it costs no PSD2 quota. Reads asset accounts from Firefly III, which include credit cards with the
credit-card role. Output on stdout, one row per bank account:

```text
BANK      ACCOUNT        NAME            CUR  STATUS       FIREFLY
swedbank  LT12…3456      Main            EUR  auto         #12 Household EUR
swedbank  hash:Wwpb…Lqc=  Card            EUR  override     #43 Credit card
revolut   LT99…0001      Revolut EUR     EUR  auto         #20 Revolut EUR
revolut   LT99…0001      Revolut USD     USD  ambiguous    #21, #22
revolut   LT99…0001      Revolut GBP     GBP  unmapped     —
old       LT55…7777      Closed          EUR  excluded     —
```

`--ids` adds a column with the full `identification_hash` for copy-paste into `accounts:` overrides
(see config.md). Exit 0 if every non-excluded account is mapped; 2 otherwise (so it can be used as a
setup check).

## Global

- `--version` prints version and exits 0.
- `--help` / `-h` on any command prints usage, exit 0.
