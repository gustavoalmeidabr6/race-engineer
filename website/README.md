# Race Engineer landing page

Static, single-page marketing site for [github.com/iamtushar324/race-engineer](https://github.com/iamtushar324/race-engineer). Published to GitHub Pages from this directory via `.github/workflows/pages.yml`.

Stack: one `index.html` + Tailwind via CDN + two SVGs. No build step, no node_modules.

## Local preview

```bash
open website/index.html
# or
python3 -m http.server -d website 8000   # then http://localhost:8000
```

## One-time setup

1. **GitHub Pages source** → repo Settings → Pages → "Build and deployment" → Source = **GitHub Actions**. The workflow in `.github/workflows/pages.yml` takes it from there.
2. **First release** → see "Cutting a release" below. The download buttons will 404 until at least one release exists.

After the first push to `main` that touches `website/**`, the site goes live at:

```
https://iamtushar324.github.io/race-engineer/
```

## How downloads work

The page links to GitHub Releases' `/releases/latest/download/<filename>` alias, which always resolves to the most recent published release. No HTML edits per release — just keep the asset filenames stable.

Expected asset filenames:

| OS | Filename |
|---|---|
| macOS (Apple silicon) | `RaceEngineer-macos-arm64.dmg` |
| Windows (x64) | `RaceEngineer-windows-x64.zip` |
| Linux (x64) | `RaceEngineer-linux-x64.AppImage` |

## Cutting a release

Using the [`gh` CLI](https://cli.github.com/):

```bash
# First release
gh release create v0.1.0 \
  ./desktop/RaceEngineer/build/bin/RaceEngineer-macos-arm64.dmg \
  --title "v0.1.0 — macOS preview" \
  --notes "First public macOS build."

# Subsequent releases — bump the tag, attach the new binaries
gh release create v0.2.0 \
  RaceEngineer-macos-arm64.dmg \
  RaceEngineer-windows-x64.zip \
  RaceEngineer-linux-x64.AppImage \
  --title "v0.2.0" \
  --notes-file RELEASE_NOTES.md

# Replace an asset on an existing release without bumping the tag
gh release upload v0.1.0 RaceEngineer-macos-arm64.dmg --clobber
```

The macOS `.dmg` doesn't exist as a build target yet — `make app` currently outputs `RaceEngineer.app`. Either zip the `.app` and rename to the expected filename, or add a `create-dmg` step to the Makefile.

## Changing the repo / owner

Edit one line near the bottom of `website/index.html`:

```js
const RELEASES_BASE = 'https://github.com/iamtushar324/race-engineer/releases/latest/download';
```

## Flipping Windows / Linux from "Coming soon" to live

In `website/index.html`:

1. Inside `#dl-win` (and `#dl-linux`) replace the wrapping `<div>` with an `<a href="#" id="dl-win">` (mirror the macOS block).
2. Below the macOS wire-up (`document.getElementById('dl-mac').href = FILES.mac;`) add the two equivalents.
3. Swap the "Coming soon" pill and disabled button styles for the active variants used on macOS.
4. In `detectOS()` handling, replace the "coming soon" hero copy with the active download path.

## Files

| File | Purpose |
|---|---|
| `index.html` | The page. Self-contained. |
| `favicon.svg` | Browser tab icon — checkered flag + red strip. |
| `og-image.svg` | Social share preview (Twitter / Slack / Discord unfurl). |
| `README.md` | This file. |
