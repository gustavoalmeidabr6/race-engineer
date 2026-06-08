.PHONY: dev start mock stop build analyst migrate-config configtool app install-app download-opencode sync-da-seed app-windows dist-windows

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
	@echo "Building bakecenterline CLI..."
	cd telemetry-core && go build -o ../workspace/bin/bakecenterline ./cmd/bakecenterline
	@echo "Build complete!"

# bake-centerlines builds just the bakecenterline tool. The operator then runs
# it manually (e.g. `./workspace/bin/bakecenterline -track 7`) after driving
# one good lap on a track to extract its racing line into workspace/tracks/.
bake-centerlines:
	cd telemetry-core && go build -o ../workspace/bin/bakecenterline ./cmd/bakecenterline
	@echo "Built ./workspace/bin/bakecenterline"
	@echo "Usage: ./workspace/bin/bakecenterline -track <id> [-lap best|last|N] [-session UID]"

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

# Download a pinned opencode binary into desktop/RaceEngineer/bin/opencode so
# the Wails .app / .exe can embed it via //go:embed. Skip if the file
# already exists and is non-empty (re-run with `make download-opencode
# FORCE=1` to bounce). darwin/arm64, linux/x64, linux/arm64 and windows/x64
# are wired here; cross-builds slot in as additional case branches.
#
# Notes on Windows: when this target runs under git bash on a Windows CI
# runner, uname -s reports MINGW64_NT or MSYS_NT and the asset OS is
# `windows`. The file inside the zip is `opencode.exe`, but the embed
# directive in app.go is the literal path `bin/opencode` (no extension —
# //go:embed treats the path as opaque bytes), so we strip the suffix on
# write. app.go appends .exe at runtime via runtime.GOOS.
OPENCODE_VERSION ?= 1.15.3
OPENCODE_BIN_PATH := desktop/RaceEngineer/bin/opencode

# Portable file-size helper: BSD stat (-f%z) doesn't exist on Linux / git
# bash, and GNU stat (-c%s) doesn't exist on macOS. `wc -c <file` works
# everywhere.
file_size = $$(wc -c < "$(1)" | tr -d ' ')

download-opencode:
	@mkdir -p $(dir $(OPENCODE_BIN_PATH))
	@if [ -s "$(OPENCODE_BIN_PATH)" ] && [ -z "$(FORCE)" ]; then \
		echo "opencode already present at $(OPENCODE_BIN_PATH) ($(call file_size,$(OPENCODE_BIN_PATH)) bytes) — pass FORCE=1 to re-download"; \
		exit 0; \
	fi
	@os_lc=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
	arch=$$(uname -m); \
	case "$$arch" in arm64|aarch64) asset_arch=arm64;; x86_64) asset_arch=x64;; *) echo "unsupported arch: $$arch"; exit 1;; esac; \
	zip_inner=opencode; \
	case "$$os_lc" in \
		darwin) asset_os=darwin;; \
		linux) asset_os=linux;; \
		mingw*|msys*|cygwin*|windows*) asset_os=windows; zip_inner=opencode.exe;; \
		*) echo "unsupported os: $$os_lc"; exit 1;; \
	esac; \
	url="https://github.com/sst/opencode/releases/download/v$(OPENCODE_VERSION)/opencode-$$asset_os-$$asset_arch.zip"; \
	echo "Downloading $$url"; \
	tmpdir=$$(mktemp -d); \
	curl -fL --retry 3 -o "$$tmpdir/opencode.zip" "$$url"; \
	unzip -q -o "$$tmpdir/opencode.zip" -d "$$tmpdir"; \
	mv "$$tmpdir/$$zip_inner" "$(OPENCODE_BIN_PATH)"; \
	chmod +x "$(OPENCODE_BIN_PATH)" 2>/dev/null || true; \
	rm -rf "$$tmpdir"; \
	echo "Installed opencode $(OPENCODE_VERSION) at $(OPENCODE_BIN_PATH) ($(call file_size,$(OPENCODE_BIN_PATH)) bytes)"

# Mirror the canonical workspace/da-workspace-seed/ into desktop/RaceEngineer/
# so //go:embed picks up the latest seed when building the .app.
sync-da-seed:
	@echo "Syncing da-workspace-seed → desktop/RaceEngineer/da-workspace-seed/"
	@rm -rf desktop/RaceEngineer/da-workspace-seed
	@cp -R workspace/da-workspace-seed desktop/RaceEngineer/da-workspace-seed

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

app: sync-da-seed download-opencode
	@echo "Building RaceEngineer.app (Wails bundle: Go core + opencode + dashboard)..."
	@# Hard gate: refuse to ship an .app without a real opencode binary.
	@# download-opencode (a prereq) is idempotent — it's a no-op when the
	@# file is already present and non-empty, and fetches the configured
	@# OPENCODE_VERSION otherwise. This assertion catches the case where
	@# the download silently failed (CI without network, GitHub 503, etc.)
	@# so a public build can never go out with the Data Analyst disabled.
	@if [ ! -s "$(OPENCODE_BIN_PATH)" ]; then \
		echo "ERROR: $(OPENCODE_BIN_PATH) is missing or 0 bytes after download-opencode."; \
		echo "       Refusing to build the .app — the Data Analyst would be disabled at runtime."; \
		echo "       Re-run with network access, or set OPENCODE_VERSION to a working release."; \
		exit 1; \
	fi
	@# Reject placeholder / suspiciously small binaries — the real darwin/arm64
	@# binary is ~85 MB, windows/x64 ~160 MB; anything under 1 MB is a stub.
	@opencode_size=$(call file_size,$(OPENCODE_BIN_PATH)); \
	if [ "$$opencode_size" -lt 1048576 ]; then \
		echo "ERROR: $(OPENCODE_BIN_PATH) is only $$opencode_size bytes — likely a placeholder."; \
		echo "       Refusing to build the .app. Run 'make download-opencode FORCE=1' to re-fetch."; \
		exit 1; \
	fi
	@echo "  ✓ opencode bundled ($(call file_size,$(OPENCODE_BIN_PATH)) bytes)"
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

# ── Windows build (driven by .github/workflows/release-windows.yml) ──────
# Mirrors the macOS `app` target. Runs on a windows-latest GH Actions
# runner under git bash. Cross-compiling from macOS is intentionally NOT
# supported — Wails requires native Windows tooling (gcc-mingw, WebView2
# headers) and the matrix runner is the path of least resistance.
APP_EXE_WIN := desktop/RaceEngineer/build/bin/RaceEngineer.exe
DIST_ZIP_WIN := desktop/RaceEngineer/build/bin/RaceEngineer-windows-x64.zip

app-windows: sync-da-seed download-opencode
	@echo "Building RaceEngineer.exe (Wails bundle: Go core + opencode + dashboard)..."
	@# Same opencode sanity gate as the macOS `app` target — refuse to ship
	@# a build where the Data Analyst would be silently disabled.
	@if [ ! -s "$(OPENCODE_BIN_PATH)" ]; then \
		echo "ERROR: $(OPENCODE_BIN_PATH) is missing or 0 bytes after download-opencode."; \
		exit 1; \
	fi
	@opencode_size=$(call file_size,$(OPENCODE_BIN_PATH)); \
	if [ "$$opencode_size" -lt 1048576 ]; then \
		echo "ERROR: $(OPENCODE_BIN_PATH) is only $$opencode_size bytes — likely a placeholder."; \
		exit 1; \
	fi
	@echo "  ✓ opencode bundled ($(call file_size,$(OPENCODE_BIN_PATH)) bytes)"
	@command -v $(WAILS) >/dev/null 2>&1 || command -v wails >/dev/null 2>&1 || { \
		echo "ERROR: wails CLI not found."; \
		echo "Install with: go install github.com/wailsapp/wails/v2/cmd/wails@latest"; \
		exit 1; \
	}
	@# Compile telemetry-core for windows/amd64 — the //go:embed directive in
	@# app.go references the literal path bin/telemetry-core (no extension),
	@# so we keep the output filename extensionless even on Windows.
	@echo "  • Compiling telemetry-core for embed (GOOS=windows GOARCH=amd64)"
	@rm -f desktop/RaceEngineer/bin/telemetry-core
	cd telemetry-core && GOOS=windows GOARCH=amd64 go build -o ../desktop/RaceEngineer/bin/telemetry-core ./cmd/server
	cd desktop/RaceEngineer && wails build -platform windows/amd64 -clean
	@echo "Built: $(APP_EXE_WIN)"

# dist-windows wraps the produced .exe in the zip filename the website
# expects: github.com/iamtushar324/race-enginer/releases/latest/download/
# RaceEngineer-windows-x64.zip
dist-windows: app-windows
	@if [ ! -f "$(APP_EXE_WIN)" ]; then \
		echo "ERROR: $(APP_EXE_WIN) not found — did make app-windows succeed?"; \
		exit 1; \
	fi
	@rm -f "$(DIST_ZIP_WIN)"
	@# `zip -j` strips directory paths so the archive root holds RaceEngineer.exe
	@# directly, matching what the website's "Download for Windows" button
	@# leads users to expect after extraction.
	cd $$(dirname "$(APP_EXE_WIN)") && zip -j "$$(basename "$(DIST_ZIP_WIN)")" "$$(basename "$(APP_EXE_WIN)")"
	@echo "Built: $(DIST_ZIP_WIN) ($(call file_size,$(DIST_ZIP_WIN)) bytes)"
