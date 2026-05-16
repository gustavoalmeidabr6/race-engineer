"""Race-engineer config loader for Python services.

This module is the Python-side counterpart of the Go schema-driven config
loader in telemetry-core/internal/config.  On import it:

  1. Tries Go's GET /api/config?reveal_secrets=1 over loopback so the
     running server is the source of truth.  Secrets are revealed only
     for 127.0.0.1 callers (the Go server enforces this).
  2. Falls back to reading ~/.race-engineer/config.json directly when the
     server isn't up yet (Python may start before Go in dev — `make start`
     orders them sequentially but be safe).
  3. Falls back to os.environ as the final tier, with a one-time warning
     so the migration is visible.

The resolved values are also written into os.environ for any existing
`os.environ.get(...)` callers that haven't been migrated yet.  This keeps
the file change list bounded — adding `import race_config` at the top of
voice_service.py / gemini_live_service.py is enough.

API:
    race_config.get(key, default=None) -> str | None
    race_config.get_int(key, default) -> int
    race_config.get_bool(key, default) -> bool
    race_config.get_float(key, default) -> float

Strict mode: setting RACE_ENGINEER_CONFIG_STRICT=1 disables the env-var
fallback entirely (matches the Go side).
"""

from __future__ import annotations

import json
import logging
import os
import sys
from pathlib import Path
from typing import Any

try:
    # urllib is in the stdlib — no extra dep.
    from urllib.request import Request, urlopen
    from urllib.error import URLError
except Exception:  # pragma: no cover — defensive only
    Request = None  # type: ignore[assignment]
    urlopen = None  # type: ignore[assignment]
    URLError = Exception  # type: ignore[assignment]


_log = logging.getLogger("race_config")
_warned_keys: set[str] = set()

_CONFIG_DIR = Path.home() / ".race-engineer"
_CONFIG_PATH = _CONFIG_DIR / "config.json"

# Strict by default: the JSON file is the authoritative source. Set
# RACE_ENGINEER_ALLOW_ENV=1 during a partial migration to re-enable the
# env-var fallback path (and the deprecation warning per resolved key).
# RACE_ENGINEER_CONFIG_STRICT=false also re-enables the fallback, kept for
# backwards compatibility with the original env-only flag.
def _resolve_strict() -> bool:
    if os.environ.get("RACE_ENGINEER_ALLOW_ENV", "").lower() in ("1", "true", "yes"):
        return False
    legacy = os.environ.get("RACE_ENGINEER_CONFIG_STRICT", "").strip().lower()
    if legacy in ("0", "false", "no"):
        return False
    return True


_STRICT = _resolve_strict()
_RACE_API_URL = os.environ.get("RACE_API_URL", "http://localhost:8081").rstrip("/")

# Loaded values keyed by config name.  Empty dict until _load() runs.
_VALUES: dict[str, Any] = {}
_LOADED = False


def _load_from_file() -> dict[str, Any]:
    if not _CONFIG_PATH.exists():
        return {}
    try:
        with _CONFIG_PATH.open("r", encoding="utf-8") as f:
            data = json.load(f)
        if isinstance(data, dict):
            return data
    except Exception as e:  # pragma: no cover — corrupt file is rare
        _log.warning("could not read %s: %s", _CONFIG_PATH, e)
    return {}


def _load_from_server() -> dict[str, Any]:
    """Pull the merged config snapshot from the running Go server.

    Secrets are returned in cleartext because the request originates from
    loopback; the server side gates this.  Returns {} on any network error
    so import never blocks the Python service from booting standalone.
    """
    if urlopen is None:
        return {}
    url = f"{_RACE_API_URL}/api/config?reveal_secrets=1"
    try:
        req = Request(url, headers={"User-Agent": "race-config/1.0"})
        with urlopen(req, timeout=1.5) as resp:
            payload = json.load(resp)
    except (URLError, OSError, json.JSONDecodeError, TimeoutError):
        return {}
    except Exception:  # pragma: no cover
        return {}
    values = payload.get("values")
    if isinstance(values, dict):
        return values
    return {}


def _hydrate_environ(values: dict[str, Any]) -> None:
    """Mirror config values into os.environ for legacy callers.

    Only writes keys that are unset OR whose current env value is empty.
    Never overwrites an explicit non-empty env var — the operator can
    still force-override via the shell.
    """
    for key, val in values.items():
        if val is None or val == "":
            continue
        current = os.environ.get(key, "")
        if current:
            continue
        if isinstance(val, bool):
            os.environ[key] = "true" if val else "false"
        else:
            os.environ[key] = str(val)


def _load() -> None:
    global _VALUES, _LOADED
    if _LOADED:
        return
    # File first (fast, always works offline).  Server can refresh secrets
    # when up.  Later sources win on collisions.
    merged: dict[str, Any] = {}
    merged.update(_load_from_file())
    server_values = _load_from_server()
    if server_values:
        merged.update(server_values)
    _VALUES = merged
    _hydrate_environ(merged)
    _LOADED = True
    if merged:
        _log.debug("race_config loaded %d keys", len(merged))


def _coerce(v: Any) -> str | None:
    if v is None:
        return None
    if isinstance(v, bool):
        return "true" if v else "false"
    return str(v)


def get(key: str, default: str | None = None) -> str | None:
    """Return the value for key as a string, or default when unset."""
    if not _LOADED:
        _load()
    if key in _VALUES and _VALUES[key] not in (None, ""):
        return _coerce(_VALUES[key])
    if _STRICT:
        return default
    val = os.environ.get(key)
    if val:
        if key not in _warned_keys:
            _warned_keys.add(key)
            _log.warning(
                "config key %s resolved from environment; persist via configtool set %s VALUE",
                key, key,
            )
        return val
    return default


def get_int(key: str, default: int) -> int:
    v = get(key)
    if v is None or v == "":
        return default
    try:
        return int(v)
    except ValueError:
        try:
            return int(float(v))
        except ValueError:
            return default


def get_float(key: str, default: float) -> float:
    v = get(key)
    if v is None or v == "":
        return default
    try:
        return float(v)
    except ValueError:
        return default


def get_bool(key: str, default: bool) -> bool:
    v = get(key)
    if v is None:
        return default
    s = v.strip().lower()
    if s in ("true", "1", "yes", "on"):
        return True
    if s in ("false", "0", "no", "off"):
        return False
    return default


def reload() -> None:
    """Re-read the config file + server snapshot.  Useful in long-running
    services that want to pick up a dashboard-edited value without a
    restart.  Static keys still need a process restart to take effect."""
    global _LOADED
    _LOADED = False
    _load()


# Auto-load at import so `import race_config` is enough to hydrate
# os.environ for legacy callers.
_load()
