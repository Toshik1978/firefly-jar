# Changelog

What changed in each `firefly-jar` release and why. Summaries are written by hand; the commit
lists under them are generated with [git-cliff](https://git-cliff.org) via
`task changelog TAG=vX.Y.Z`.

Versions follow [semver](https://semver.org). Commits follow
[Conventional Commits](https://www.conventionalcommits.org).

---

## v1.0.0 — 2026-09-23

The first release. `firefly-jar` is a one-shot command that cron runs on a Linux server: it fetches
recent bank transactions through Enable Banking, compares them with a self-hosted Firefly III
instance, and sends a Telegram and/or email digest naming every bank transaction nobody entered. It
is a reminder, not an importer. You still enter every transaction by hand.

**Read-only on both sides, by construction.** The Firefly III client can only send `GET` requests;
its transport rejects any other method before it leaves the process. At the bank it holds
account-information consent only, never calls a payment endpoint, and never claims a person is
present.

**Nothing slips through silently.** Every bank transaction in the window ends up matched, missing,
deduplicated (a pending copy of a booked one) or void, and an account that could not be checked says
why in the digest. A missing transaction that is close to an existing entry carries a hint, so a
mistyped date is easy to tell from a real gap.

**Made for cron.** One invocation is one run. A clean run is silent; exit code 1 means something is
missing and 2 means the run was incomplete. `auth <bank>` connects a bank and the digest warns
before its consent expires; `accounts` shows how each bank account maps to Firefly III, and
`accounts:` overrides cover what IBAN matching cannot.

Prebuilt archives below cover linux and darwin on amd64 and arm64. Start with the README's Install
and Configure sections.

### Features

- feat: add the configuration, state, logging and bank-provider foundations ([867ccf1](https://github.com/Toshik1978/firefly-jar/commit/867ccf1b31e7dac001af131ddcd550350b4c9d14))
- feat: remind about bank transactions missing from Firefly III ([61dc0db](https://github.com/Toshik1978/firefly-jar/commit/61dc0dbec7e362d6dd315a4e9161d614f9abb611))
- feat: connect a bank and warn before its consent expires ([c212520](https://github.com/Toshik1978/firefly-jar/commit/c2125203c87fc8c4539356739a9afc4aaf7aa3b3))
- feat: add account overrides and the read-only accounts command ([5f7c4db](https://github.com/Toshik1978/firefly-jar/commit/5f7c4dbd05b275c62dbdce5b4ab6b4df91c45633))
- feat: report check failures with per-bank isolation, delivery fallback and fail-fast validation ([102e975](https://github.com/Toshik1978/firefly-jar/commit/102e975fd69dde1b6183d04946b8f4e9b0ebf8c3))
- feat: name the Firefly III account in each digest account heading ([2d4ff4e](https://github.com/Toshik1978/firefly-jar/commit/2d4ff4efaee16111bf83f0f117119923a9a1ac6b))

### Others

- docs: specify the missing-transaction reminder with Spec Kit ([d7c13e8](https://github.com/Toshik1978/firefly-jar/commit/d7c13e8b4e1e2912b65e23c5692581e90554ddec))
- refactor: adopt retry-go and httpmock and name packages for what they hold ([c017b06](https://github.com/Toshik1978/firefly-jar/commit/c017b06e91aa2f672027df4dc2de7c69f790ae6c))
- ci: publish coverage and test badges and gate coverage at 90% ([9789fb2](https://github.com/Toshik1978/firefly-jar/commit/9789fb28e7faadf72fc95a173198bd26dc98df0f))

---

