"""Pi agent service — sandboxed background data-analyst team.

Launched by start.sh when PI_AGENT_MODE != "off". Connects to the Go MCP
server (telemetry-core) at ${RACE_API_URL}${PI_AGENT_MCP_PATH} and runs the
planner long-poll loop. There is no shell, no requests/httpx, no subprocess,
and no arbitrary file I/O — the only outward connections are the MCP session
and the LLM provider SDK's outbound. The AST sandbox check at
tests/test_pi_agent_sandbox.py enforces the absent-imports rule on CI.
"""

from __future__ import annotations

import asyncio
import logging
import os
import sys
from pathlib import Path

# Ensure the project root is on sys.path so `python pi_agent_service.py` works
# from any cwd. The pkg lives under ./python/pi_agent.
ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT / "python"))

# Hydrate env from the persisted ~/.race-engineer/config.json (and the running
# Go server at /api/config). Same pattern as gemini_live_service.py.
import race_config  # noqa: F401  (side-effect import)

from pi_agent import planner


def main() -> None:
    log_level = os.environ.get("LOG_LEVEL", "info").upper()
    logging.basicConfig(
        level=getattr(logging, log_level, logging.INFO),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        datefmt="%H:%M:%S",
    )
    log = logging.getLogger("pi_agent")
    cfg = planner.from_env()
    log.info("pi_agent starting (mcp_url=%s provider=%s)", cfg.mcp_url, cfg.provider)
    try:
        asyncio.run(planner.run_forever(cfg))
    except KeyboardInterrupt:
        log.info("pi_agent stopped (SIGINT)")


if __name__ == "__main__":
    main()
