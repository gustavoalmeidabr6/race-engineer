.PHONY: dev start mock stop build analyst migrate-config configtool app install-app

# Recommended developer command — Go telemetry-core + Vite, no Python.
# After Phase 3 the runtime is pure Go (TTS/STT/Live all in-binary), so
# this is the lean 2-process launcher with clean Ctrl+C shutdown.
dev:
	@echo "Starting Race Engineer (dev)..."
	./dev.sh

# Legacy launcher — also spins up the Python voice/Live services. Keep
# around for A/B comparison until the Go Live path is fully hardened
# (see todos.md for the remaining parity gaps).
start:
	@echo "Starting Race Engineer (legacy with Python services)..."
	./start.sh

# Same as `make dev`, but forces the telemetry core into mock mode so the
# system runs end-to-end without an F1 25 game pushing UDP packets.
mock:
	@echo "Starting Race Engineer in MOCK mode (no game required)..."
	TELEMETRY_MODE=mock ./dev.sh

stop:
	@echo "Stopping Race Engineer..."
	./stop.sh

build:
	@echo "Building Go telemetry-core..."
	cd telemetry-core && go build -o ../workspace/bin/telemetry-core cmd/server/main.go
	@echo "Building racedb query tool..."
	cd telemetry-core && go build -o ../workspace/bin/racedb cmd/query/main.go
	@echo "Building insightlog tool..."
	cd telemetry-core && go build -o ../workspace/bin/insightlog cmd/insightlog/main.go
	@echo "Building buttonprobe tool..."
	cd telemetry-core && go build -o ../workspace/bin/buttonprobe cmd/buttonprobe/main.go
	@echo "Building buttonwatch tool..."
	cd telemetry-core && go build -o ../workspace/bin/buttonwatch cmd/buttonwatch/main.go
	@echo "Building configtool CLI..."
	cd telemetry-core && go build -o ../workspace/bin/configtool ./cmd/configtool
	@echo "Build complete!"

# Build just the configtool binary so it's available before `make build`.
configtool:
	cd telemetry-core && go build -o ../workspace/bin/configtool ./cmd/configtool

# One-shot migration from .env → ~/.race-engineer/config.json. Idempotent:
# re-running keeps existing JSON keys unless --force is passed. The .env file
# is renamed to .env.migrated-<unix> on success so it isn't auto-imported
# next time.
migrate-config: configtool
	@./workspace/bin/configtool import-env --env-file .env || true

analyst:
	@echo "Starting OpenCode analyst agent on port $${OPENCODE_PORT:-4095}..."
	cd workspace && opencode serve --port $${OPENCODE_PORT:-4095}

# ── Desktop app (Wails bundle) ───────────────────────────────────────────
# The user-facing "Race Engineer" Mac app lives at desktop/RaceEngineer/
# and is a Wails wrapper that embeds the React dashboard via go:embed.
# `make app` produces desktop/RaceEngineer/build/bin/RaceEngineer.app;
# `make install-app` swaps it into /Applications/RaceEngineer.app.
# Requires the wails CLI (https://wails.io/docs/gettingstarted/installation):
#   go install github.com/wailsapp/wails/v2/cmd/wails@latest
WAILS ?= $(HOME)/go/bin/wails
APP_BUNDLE := desktop/RaceEngineer/build/bin/RaceEngineer.app
INSTALL_DEST := /Applications/RaceEngineer.app

app:
	@echo "Building RaceEngineer.app (Wails bundle: Go core + embedded dashboard)..."
	@command -v $(WAILS) >/dev/null 2>&1 || { \
		echo "ERROR: wails CLI not found at $(WAILS)."; \
		echo "Install with: go install github.com/wailsapp/wails/v2/cmd/wails@latest"; \
		exit 1; \
	}
	@# Build the standalone telemetry-core binary that desktop/RaceEngineer/app.go
	@# pulls in via //go:embed. Without this step the .app boots with a 0-byte
	@# binary, the embedded server never starts, and the dashboard sits on
	@# "starting telemetry core" forever — symptom: 60+ /health retries.
	@echo "  • Compiling telemetry-core for embed → desktop/RaceEngineer/bin/telemetry-core"
	@rm -rf desktop/RaceEngineer/bin/telemetry-core
	cd telemetry-core && go build -o ../desktop/RaceEngineer/bin/telemetry-core ./cmd/server
	cd desktop/RaceEngineer && $(WAILS) build -clean
	@echo "Built: $(APP_BUNDLE)"

# install-app rebuilds the bundle and swaps it into /Applications.
# Safe to run while the running stack (make start / make dev) is up — the
# installed app is a separate desktop process, not used by `make start`.
install-app: app
	@if pgrep -f "$(INSTALL_DEST)/Contents/MacOS/RaceEngineer" >/dev/null 2>&1; then \
		echo "Quitting the running RaceEngineer.app first…"; \
		osascript -e 'quit app "RaceEngineer"' || true; \
		sleep 1; \
	fi
	rm -rf "$(INSTALL_DEST)"
	cp -R "$(APP_BUNDLE)" "$(INSTALL_DEST)"
	@echo "Installed: $(INSTALL_DEST)"
	@/usr/libexec/PlistBuddy -c "Print :CFBundleShortVersionString" "$(INSTALL_DEST)/Contents/Info.plist" \
		| awk '{print "Version: " $$0}'
