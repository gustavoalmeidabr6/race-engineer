#!/bin/bash
# Wails frontend hook: builds the React dashboard from ../../dashboard
# and syncs dist/ into desktop/RaceEngineer/frontend/dist for go:embed.
# Invoked by `wails build` / `wails dev` with cwd = frontend/.
set -euo pipefail

cd "$(dirname "$0")"                       # → desktop/RaceEngineer
DASHBOARD="$(cd ../../dashboard && pwd)"
EMBED_DIST="$(pwd)/frontend/dist"

case "${1:-build}" in
  install)
    (cd "$DASHBOARD" && npm install)
    ;;
  build)
    (cd "$DASHBOARD" && npm run build)
    rm -rf "$EMBED_DIST"
    mkdir -p "$EMBED_DIST"
    cp -R "$DASHBOARD/dist/." "$EMBED_DIST/"
    ;;
  dev)
    # `wails dev` already proxies to frontend:dev:serverUrl, so just run
    # vite in the dashboard. Use exec so SIGTERM from wails reaches it.
    cd "$DASHBOARD" && exec npm run dev
    ;;
  *)
    echo "usage: $0 {install|build|dev}" >&2
    exit 2
    ;;
esac
