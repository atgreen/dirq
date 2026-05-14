---
name: make-release
description: Cut a new DirQ release. Updates CHANGELOG.md, verifies build and tests pass, commits, tags, and pushes. Use when asked to "release", "cut a release", "bump version", or "tag a new version".
argument-hint: "[version]"
disable-model-invocation: true
allowed-tools: Bash Read Edit Write Grep Glob
---

# Release DirQ

Follow these steps exactly, stopping on any failure.

## 1. Determine version

If the user provided a version (`$ARGUMENTS`), validate that it matches `MAJOR.MINOR.PATCH` where each component is a non-negative integer (e.g. `0.2.0`). If it doesn't, stop and tell the user.

If no version was provided, suggest one:

1. Read the current version from the latest `## [X.Y.Z]` entry in `CHANGELOG.md`.
2. Review commits since the last tag:
   ```bash
   git log $(git describe --tags --abbrev=0)..HEAD --oneline
   ```
3. Apply semver rules:
   - **MAJOR** bump: commits contain breaking changes (removed features, changed APIs, incompatible behavior)
   - **MINOR** bump: commits add new features, new commands, new capabilities
   - **PATCH** bump: commits are only bug fixes, documentation, or internal improvements
4. Present the suggested version with a one-line rationale and ask the user to confirm or override.

Use the confirmed version as VERSION for all subsequent steps.

**Check the tag doesn't already exist:**
```bash
git tag -l "vVERSION"
```
If output is non-empty, stop — this version has already been released.

**Ensure clean working tree:**
```bash
git status --porcelain
```
If there are staged or unstaged changes to **tracked** files, stop and ask the user to commit or stash first.

Then check untracked files. If any look like they don't belong in the repo (log files, binaries, build artifacts, temp files, editor backups, etc.), list them and ask the user whether to clean up, add to `.gitignore`, or proceed anyway. Benign untracked files (e.g. `.claude/`, `.dirq-aws-state/`, local config) are fine to ignore silently.

**Verify git identity:**
```bash
git config user.email
```
If the result is not `green@moxielogic.com`, stop and tell the user their git email is misconfigured for this repository.

**Ensure we're on main and synced with remote:**
```bash
git branch --show-current
git fetch origin main
git rev-list HEAD..origin/main --count
```
If the branch is not `main`, or there are upstream commits not yet pulled, stop and tell the user.

**Check CI is green on HEAD:**
```bash
gh run list --branch main --limit 1 --json conclusion --jq '.[0].conclusion'
```
If the result is not `success`, stop and warn the user that CI is failing on main. Ask whether to proceed anyway.

## 2. Build and test

```bash
go build ./... 2>&1
```

If the build fails, stop and report the failure. Do NOT continue.

```bash
go test ./... 2>&1
```

If any tests fail, stop and report the failure. Do NOT continue.

## 3. Update CHANGELOG.md

Add a new version section to `CHANGELOG.md` immediately after the `and this project adheres to [Semantic Versioning]` line.

To decide what goes in the notes, diff against the most recent tag:

```bash
git log $(git describe --tags --abbrev=0)..HEAD --oneline
```

Include ONLY user-facing changes, organized under these headings as applicable:

- **Added** — new features and capabilities
- **Changed** — changes to existing features
- **Fixed** — bug fixes
- **Removed** — removed features

Do NOT include internal changes (refactors, lint fixes, CI changes, directory reorganization). Those are visible in the git log for anyone who needs them.

Match the voice and format of previous entries in `CHANGELOG.md`.

Add the version link at the bottom of the file:
```
[VERSION]: https://github.com/atgreen/dirq/releases/tag/vVERSION
```

The release workflow (`.github/workflows/release.yml`) extracts notes from CHANGELOG.md automatically when creating the GitHub release, so this is the only place release notes need to be written.

## 4. Commit

```bash
git add CHANGELOG.md
git commit -m "Release VERSION"
```

## 5. Tag and push

Ask the user for confirmation before pushing, then:

```bash
git tag -s vVERSION -m "Release VERSION"
git push origin main
git push origin vVERSION
```

Pushing the tag triggers GitHub Actions which automatically builds cross-platform binaries (Linux amd64/arm64, Windows amd64, macOS amd64/arm64), creates the GitHub release with notes extracted from CHANGELOG.md, and publishes RPMs.
