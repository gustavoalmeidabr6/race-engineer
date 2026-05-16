"""Sandbox AST test for the pi agent.

The pi agent's threat model is "stop the LLM doing surprising things". We
enforce that structurally, by ensuring its source files don't import the
escape hatches it would need to break out of the MCP-only contract:

  - subprocess / pty / pexpect → no shell
  - requests / httpx / urllib / socket → no arbitrary HTTP
  - os.system / os.popen → no shell
  - shutil → no fs side-effects beyond the logger

Failing this test means someone added a banned import; either remove it or
update this test (with justification) and the corresponding section in
CLAUDE.md and the plan.
"""

from __future__ import annotations

import ast
import pathlib

ROOT = pathlib.Path(__file__).resolve().parent.parent

# Files the test scans. Add new pi-agent module files here as they're added.
TARGETS = [
    ROOT / "pi_agent_service.py",
    ROOT / "python" / "pi_agent" / "__init__.py",
    ROOT / "python" / "pi_agent" / "mcp_client.py",
    ROOT / "python" / "pi_agent" / "planner.py",
    ROOT / "python" / "pi_agent" / "specialist.py",
]

BANNED_TOP_LEVEL = {
    "subprocess",
    "pty",
    "pexpect",
    "requests",
    "httpx",
    "urllib",
    "socket",
    "shutil",
}

# Banned attribute calls that look benign at import time (e.g. os.system).
BANNED_ATTR_CALLS = {
    "system",   # os.system
    "popen",    # os.popen
    "fork",     # os.fork
    "exec",     # os.exec*
    "spawnl", "spawnv", "spawnve",
}


def _imports(tree: ast.AST) -> set[str]:
    found: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                found.add(alias.name.split(".")[0])
        elif isinstance(node, ast.ImportFrom):
            if node.module:
                found.add(node.module.split(".")[0])
    return found


def _attr_calls(tree: ast.AST) -> set[str]:
    found: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute):
            found.add(node.func.attr)
    return found


def test_no_banned_imports() -> None:
    offenders: list[str] = []
    for path in TARGETS:
        assert path.exists(), f"sandbox target missing: {path}"
        tree = ast.parse(path.read_text(), filename=str(path))
        imps = _imports(tree)
        bad = imps & BANNED_TOP_LEVEL
        if bad:
            offenders.append(f"{path.relative_to(ROOT)}: {sorted(bad)}")
    assert not offenders, "banned imports found:\n  " + "\n  ".join(offenders)


def test_no_banned_attr_calls() -> None:
    offenders: list[str] = []
    for path in TARGETS:
        tree = ast.parse(path.read_text(), filename=str(path))
        attrs = _attr_calls(tree) & BANNED_ATTR_CALLS
        if attrs:
            offenders.append(f"{path.relative_to(ROOT)}: {sorted(attrs)}")
    assert not offenders, "banned attribute calls found:\n  " + "\n  ".join(offenders)
