"""Pi agent — sandboxed background data-analyst team.

The agent runs as a separate Python process (pi_agent_service.py) launched by
start.sh when PI_AGENT_MODE != "off". Its only outward connection is the MCP
endpoint exposed by telemetry-core (see internal/mcp/server.go). It deliberately
does NOT import subprocess, requests, httpx, or open any files — the AST
sandbox check at tests/test_pi_agent_sandbox.py enforces this.
"""
