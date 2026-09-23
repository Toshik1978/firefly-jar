# Working on firefly-jar

**Pull requests and issues are not accepted** (see the [README](README.md#about-this-project)). This
guide is for anyone working on a fork or a local checkout: how to set up, what the gate checks, and
the conventions the code follows. The full rule set for agents working on the code is in
[`.claude/CLAUDE.md`](.claude/CLAUDE.md); where the two differ, that file wins.

## Prerequisites

- [mise](https://mise.jdx.dev) installs the toolchain pinned in `.mise.toml`: Go, golangci-lint,
  GoReleaser and git-cliff.
- [Task](https://taskfile.dev) runs the project's commands (`Taskfile.yml`).
- [pre-commit](https://pre-commit.com) runs the hooks in `.pre-commit-config.yaml`.

## Getting started

```bash
task setup           # mise install, go mod download
pre-commit install   # commit-msg, pre-commit and pre-push hooks
task check           # format:check, lint, test: must pass before every commit
```

`task build` produces a static `./firefly-jar`. `task cover:check` runs the tests with coverage and
fails below the floor in `Taskfile.yml`. `task audit` runs govulncheck.

## The gate

`task check` runs `format:check`, `lint` and `test` (with the race detector), and must exit 0 before
every commit. CI runs the same checks, plus the coverage floor, govulncheck and the release config
check. Never pass `--no-verify`. `.golangci.yml` is the author's standard configuration and is not
edited to make a change pass.

Tests never call a live service. Live smoke tests exist only behind the `live` build tag and need a
real configuration.

## Conventions

- **Commits** follow [Conventional Commits](https://www.conventionalcommits.org) (`feat:`, `fix:`,
  `docs:`, `refactor:`, `test:`, `ci:`, `chore:`), enforced by the `commit-msg` hook. The changelog is
  generated from them, so the subject is what a user reads in the release notes.
- **Features** are planned with [Spec Kit](https://github.com/github/spec-kit) (specify, clarify,
  plan, tasks, analyze) and executed with [Superpowers](https://github.com/obra/superpowers), one
  test-first task at a time. The artifacts live in `specs/<NNN-name>/`, and
  `.specify/memory/constitution.md` wins every conflict.
- **Tests first.** A behavior change starts with a test that fails for the right reason. Tests are
  testify suites with one `Test<Package>` entry point per package; HTTP adapters use per-client
  `httpmock` transports and anonymized fixtures; timing uses `testing/synctest`, never a real sleep.
- **Dependencies.** Standard library first. A new direct dependency needs the author's approval.
- **Money is never a float** (`money.Amount`), and dates are civil dates (`civil.Date`).

## Invariants

These are enforced by tests, and no change may relax them:

1. Read-only against Firefly III: `GET` only, rejected in the transport otherwise.
2. Read-only at the bank: account-information consent, no payment endpoints, no `Psu-*` headers.
3. Nothing is dropped silently: every bank transaction in the window is accounted for.
4. Quiet when clean: a run that exits 0 or 1 writes nothing to stdout or stderr.
5. One invocation is one run: no scheduler, no daemon, no HTTP server.

[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) explains why.

## Never commit live data

The repository is public. No tracked file, commit message or `.beads/` entry may hold data from a
real run: bank names in use, IBANs or fragments of them, account ids, merchants, amounts, hosts,
chat ids, emails, tokens or local filesystem paths. Examples use `example.com`, masked IBANs such as
`LT12…3456`, and zeroed UUIDs.

## Releases

Releases are cut by pushing a `v*` tag. The steps are in [`docs/RELEASING.md`](docs/RELEASING.md).
