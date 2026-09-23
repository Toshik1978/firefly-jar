# Releasing

How to cut a release of `firefly-jar`. The release machinery (`task changelog`,
`task release:check`, `task release:snapshot`, `task release:verify`, `.cliff.toml`,
`.goreleaser.yaml`, `CHANGELOG.md` and `.github/workflows/release.yml`) matches this document;
change them together.

Tagging and pushing are the author's act. Never push with `--force` to a shared branch.

## How versions work

The git tag is the version. Nothing else in the repository carries a version number:

- GoReleaser names the release and its archives after the tag.
- `firefly-jar --version` reports the version the Go toolchain stamps into the binary from git,
  which in a release checkout is the tag itself. There is no `-X` ldflag. A build that is not on a
  tag reports a pseudo-version, and `go run` or a test binary reports `(devel)`.
- `CHANGELOG.md`'s first `## vX.Y.Z` heading names the release the tree is ready to publish.

Versions follow [semver](https://semver.org). **A `v2.0.0` or later tag needs the module path to
end in `/v2` first** (`go.mod` and every import). Go refuses a v2+ tag on a module path without
that suffix and stamps a pseudo-version instead, and the release workflow then stops before
publishing because the binary's version does not match the tag.

## Step 0 — before the first push only: sweep the whole history

The repository is public from the first push. Before it, check that no commit anywhere in the
history carries live data: a bank name in use, an IBAN or a fragment of one, an account id, a
merchant, an amount or date from a real transaction, a host, a chat id, an email address, a token
or a local filesystem path. `.beads/issues.jsonl` is tracked, so every past version of it counts.

```bash
git log -p --all | grep -nE '<pattern>'   # one pattern per identifier you know you used
```

Run it once per identifier from your real configuration, and read every hit. Once the history is
public this step cannot be undone, which is why it comes before anything else.

Then create the GitHub repository, add the `GIST_ID` and `GIST_SECRET_TOKEN` secrets the coverage
badges need (see `.github/workflows/ci.yml`), and push `main`.

## Step 1 — generate the changelog entry

```bash
task changelog TAG=vX.Y.Z
```

git-cliff prepends a `## vX.Y.Z — <date>` section to `CHANGELOG.md`, listing the commits since the
last tag grouped as Features, Bug Fixes and Others. `chore`, `test`, `style`, `build(deps)` and
`ci(deps)` commits are left out. Once a tag exists, `task changelog` with no `TAG` derives the next
version from the commits; the first release must name it.

Write a short paragraph above the generated groups: what the release means for someone running
`firefly-jar`, not a restatement of the commit list. Mention anything an existing user must do,
such as a config key that changed.

## Step 2 — preview the release body

```bash
.github/scripts/extract-changelog.sh vX.Y.Z
```

This prints exactly the text the release workflow will publish as the GitHub release body: the
section from `## vX.Y.Z` to the next `## ` heading. It exits non-zero when the section is missing,
which is the guard against publishing a release with empty notes.

## Step 3 — commit the changelog

```bash
git add CHANGELOG.md
task check
git commit -m "chore(release): vX.Y.Z"
git push origin main
```

The `chore(release)` subject keeps this commit out of the next release's notes. CI's
`release-config` job checks the GoReleaser config and that this entry extracts, so a broken
release config fails here rather than after the tag is public.

## Step 4 — dry run, tag and push

```bash
task release:check                 # goreleaser check
task release:snapshot              # build all four archives into dist/, publish nothing
task release:verify TAG=vX.Y.Z     # the tag is the current changelog entry
git tag -a vX.Y.Z -m vX.Y.Z
git push origin vX.Y.Z
```

Look inside one `dist/*.tar.gz` before tagging: it must hold `firefly-jar`, `LICENSE`,
`LICENSE.civil`, `README.md` and `config.example.yaml`. The snapshot's binary reports a
pseudo-version, because the snapshot is built before the tag exists.

Pushing the tag starts the release workflow. It:

1. runs `task check` and `task audit` on the tagged commit;
2. checks the tag is the current `CHANGELOG.md` entry and extracts its section as the release body;
3. builds the binary once and fails unless `firefly-jar --version` prints exactly the tag;
4. publishes `tar.gz` archives for linux and darwin on amd64 and arm64, plus a checksums file,
   with GoReleaser.

A second job then reads the published release back and fails if its body is empty. GoReleaser takes
`--release-notes` through its changelog pipe, so disabling that pipe would publish working archives
with no notes while every earlier step passes; only reading the release back can catch it.

## Retrying a failed release

If the workflow fails after the tag is pushed, fix the cause on `main`, then delete the tag and
push it again:

```bash
git tag -d vX.Y.Z
git push origin :refs/tags/vX.Y.Z
git tag -a vX.Y.Z -m vX.Y.Z
git push origin vX.Y.Z
```

If GoReleaser already created the GitHub release, delete it first (`gh release delete vX.Y.Z`), so
the second run does not collide with the assets the first one uploaded. Delete and recreate the tag
rather than force-pushing it: a force-push of a tag that still points at the same commit triggers
nothing.
