# GitHub Pages: docs + package repos on one site

`atgreen.github.io/dirq/` serves **two** things from the **`gh-pages` branch**:

| Path | Produced by | Trigger |
|------|-------------|---------|
| `/` and the docs tree (`/tutorials/`, `/how-to/`, `/reference/`, `/explanation/`) | `.github/workflows/docs.yml` (MkDocs) | push to `main` touching `docs/**`, `mkdocs.yml`, `overrides/**`; or manual dispatch |
| `/rpm-repo/`, `/deb-repo/` | `.github/workflows/release.yml` → `deploy-pages` job | push of a `v*` tag |

Both publish with **`peaceiris/actions-gh-pages@v4` and `keep_files: true`**, so each writes only its own subtree and never deletes the other's. The docs never touch `rpm-repo/`/`deb-repo/`; a release never touches the docs.

## One-time activation

The repo currently serves Pages via the **GitHub Actions** method (the old
`deploy-pages` job uploaded `docs/` as a Pages artifact). Switching to the
`gh-pages` branch must be done in an order that never drops the live package
URLs (`baseurl=https://atgreen.github.io/dirq/rpm-repo`, etc.).

**Do these in order:**

1. **Merge this branch to `main`.**
   `docs.yml` runs and creates the `gh-pages` branch containing the MkDocs
   site. Pages is *still* served by the old Actions method at this point, so
   the live site and package URLs are unchanged. `gh-pages` now has docs but
   **no repos yet** — do not flip the Pages source.

2. **Seed the package repos onto `gh-pages`.** Cut a patch release (push a
   `v*` tag). `release.yml`'s `deploy-pages` job publishes `rpm-repo/` and
   `deb-repo/` to `gh-pages` with `keep_files: true`, preserving the docs
   already there. `gh-pages` now has **docs + repos**.
   *(Alternative, no release: check out `gh-pages`, mirror the current live
   `rpm-repo/` and `deb-repo/` trees into it — e.g. `wget -r -np` from the
   live site — commit, push.)*

3. **Flip the Pages source.** Repo **Settings → Pages → Build and deployment
   → Source: "Deploy from a branch" → Branch: `gh-pages` → `/(root)` → Save.**

4. **Verify** these all resolve:
   - `https://atgreen.github.io/dirq/` — the docs home
   - `https://atgreen.github.io/dirq/rpm-repo/repodata/repomd.xml`
   - `https://atgreen.github.io/dirq/deb-repo/dists/stable/Release`
   - a smoke test: `dnf install dirq-agent` and `apt install dirq-agent`

## Steady state

- Edit docs under `docs/` → push to `main` → `docs.yml` redeploys the site.
- Cut a `v*` release → `release.yml` refreshes the package repos.
- Local preview: `pip install mkdocs-material && mkdocs serve`.

## Rollback

Set **Settings → Pages → Source** back to **GitHub Actions** and revert the
`release.yml` change in this branch (restoring the `upload-pages-artifact` /
`deploy-pages` job), then re-run a release to repopulate the Actions-method
deployment.
