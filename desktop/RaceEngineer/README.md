# RaceEngineer.app — Wails desktop wrapper

This directory builds the user-facing **Race Engineer** Mac app that ships
to `/Applications/RaceEngineer.app`. It is a thin [Wails](https://wails.io/)
shell that:

- Boots an embedded copy of the Go `telemetry-core` server (same code the
  `make start` flow uses, linked into the desktop process).
- Embeds the built React dashboard from `../../dashboard/dist/` via Go
  `embed` so the .app is a single self-contained bundle.
- Renders that dashboard in a native WebKit window.

The dev `make start` / `make dev` flow does **not** use this wrapper — it
runs the Go server as a CLI process and serves the dashboard via Vite on
`http://localhost:8092`. The wrapper exists so non-developers can
double-click an app icon instead of running scripts.

## Layout

| Path | Purpose |
|---|---|
| `main.go`, `app.go` | Wails entrypoint + bootstrap of the embedded telemetry-core. |
| `frontend/` | Wails-required folder; `wailsjs/` stubs + `dist/` populated by `build-frontend.sh`. |
| `build-frontend.sh` | Hook invoked by Wails: builds `../../dashboard` and copies `dist/` here for `go:embed`. |
| `wails.json` | Wails project config — name, version, frontend-build hook. |
| `workspace-seed/` | Default workspace files shipped inside the bundle for first-run. |
| `build/bin/RaceEngineer.app` | Output bundle produced by `wails build`. |

## Build & install

From the **repo root** (not this directory):

```bash
make app          # produce desktop/RaceEngineer/build/bin/RaceEngineer.app
make install-app  # rebuild AND copy into /Applications/RaceEngineer.app
```

`make install-app` quits the running `.app` first (via `osascript`) so the
replacement doesn't fail with "file busy". It does not touch the dev
workflow — `make start` keeps running independently.

## Requirements

- [Wails v2 CLI](https://wails.io/docs/gettingstarted/installation):
  `go install github.com/wailsapp/wails/v2/cmd/wails@latest` (lands in
  `~/go/bin/wails`, which is what the Makefile expects). Override the
  path with `WAILS=…` if your install is elsewhere.
- Xcode command-line tools (`xcode-select --install`) for the codesign
  step Wails runs at the end of a build.

## Updating the version

Bump `info.productVersion` in `wails.json`; Wails writes it into
`Contents/Info.plist`. `make install-app` echoes the installed version at
the end so you can confirm the swap landed.

## Common gotchas

- `npm run build` in `dashboard/` uses `tsc -b` (project references),
  which is stricter than `tsc --noEmit`. A clean type-check during
  development can still fail the Wails frontend build — always do a
  trial `npm run build` after touching `dashboard/`.
- The Wails frontend hook (`build-frontend.sh`) clones `dashboard/dist`
  into `frontend/dist`; do not edit `frontend/dist` directly, it's
  regenerated on every build.
