"""MCP client wrapper used by the pi agent.

Wraps the official `mcp` Python SDK's StreamableHTTP transport so the rest of
the agent code can call tools by name without touching transport details.
"""

from __future__ import annotations

import asyncio
import contextvars
import json
import logging
from contextlib import AsyncExitStack
from dataclasses import dataclass, field
from typing import Any

from mcp import ClientSession
from mcp.client.streamable_http import streamablehttp_client

log = logging.getLogger("pi_agent.mcp_client")


# current_run_id / current_persona are set by the planner when it dispatches
# a specialist task. MCPClient.call reads them and injects `_run_id` / `_persona`
# into every tool-call arg dict so the Go MCP hook can group activities per
# run. ContextVars are task-local under asyncio, so concurrent specialists
# never see each other's values.
current_run_id: contextvars.ContextVar[str] = contextvars.ContextVar(
    "pi_agent_run_id", default=""
)
current_persona: contextvars.ContextVar[str] = contextvars.ContextVar(
    "pi_agent_persona", default=""
)


@dataclass
class MCPClient:
    """Async MCP client over streamable-HTTP. Single session per process."""

    url: str
    _session: ClientSession | None = None
    _stack: AsyncExitStack = field(default_factory=AsyncExitStack)
    _tools: list[dict[str, Any]] = field(default_factory=list)

    async def connect(self) -> None:
        log.info("connecting to MCP server at %s", self.url)
        try:
            read, write, _ = await self._stack.enter_async_context(
                streamablehttp_client(self.url)
            )
            self._session = await self._stack.enter_async_context(ClientSession(read, write))
            await self._session.initialize()
            await self._refresh_tools()
        except BaseException:
            # Roll back any half-entered context managers so the caller can
            # retry with a clean stack. Without this, AsyncExitStack.aclose()
            # re-raises the original ConnectError during unwind and masks the
            # retry loop's intent.
            try:
                await self._stack.aclose()
            except BaseException:
                pass
            self._stack = AsyncExitStack()
            self._session = None
            self._tools = []
            raise
        log.info("MCP connected — %d tools available", len(self._tools))

    async def _refresh_tools(self) -> None:
        if self._session is None:
            return
        resp = await self._session.list_tools()
        self._tools = [
            {
                "name": t.name,
                "description": t.description or "",
                "input_schema": t.inputSchema or {"type": "object"},
            }
            for t in resp.tools
        ]

    @property
    def tools(self) -> list[dict[str, Any]]:
        return list(self._tools)

    async def call(self, name: str, args: dict[str, Any] | None = None) -> str:
        """Invoke a tool and return the textual result.

        Errors come back to the LLM as text rather than raising — the model is
        better at recovering from a clear error message than from an exception.
        """
        if self._session is None:
            raise RuntimeError("MCP session not connected")
        # Inject the per-task run_id / persona so the server hook can group
        # this tool call under the right specialist dispatch. ContextVar
        # values default to "" outside a planner-dispatched task — those
        # calls land in the "system" bucket on the dashboard.
        payload: dict[str, Any] = dict(args) if args else {}
        rid = current_run_id.get()
        if rid and "_run_id" not in payload:
            payload["_run_id"] = rid
        per = current_persona.get()
        if per and "_persona" not in payload:
            payload["_persona"] = per
        log.debug("→ %s args=%s", name, payload)
        result = await self._session.call_tool(name, payload)
        # mcp Python SDK returns content as a list of content parts.
        parts: list[str] = []
        for part in result.content or []:
            text = getattr(part, "text", None)
            if text is not None:
                parts.append(text)
            else:
                parts.append(json.dumps(getattr(part, "model_dump", lambda: {})()))
        out = "\n".join(parts)
        if result.isError:
            log.warning("← %s ERROR %s", name, out[:200])
        else:
            log.debug("← %s %d chars", name, len(out))
        return out

    async def close(self) -> None:
        # Best-effort cleanup. The streamable-HTTP task group lives inside an
        # anyio cancel scope; if shutdown runs in a different async task than
        # the one that entered the scope (common when the planner is cancelled
        # by a SIGTERM), anyio raises "Attempted to exit cancel scope in a
        # different task". Swallow it — the process is going away anyway.
        try:
            await self._stack.aclose()
        except BaseException as e:
            log.debug("MCP close suppressed (%s: %s)", type(e).__name__, e)


async def smoke_list(url: str) -> None:
    """Standalone helper: `python -m pi_agent.mcp_client URL` → list tools."""
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")
    client = MCPClient(url=url)
    try:
        await client.connect()
        for t in client.tools:
            print(f"- {t['name']}: {t['description'][:120]}")
    finally:
        await client.close()


if __name__ == "__main__":
    import sys

    target = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8081/mcp"
    asyncio.run(smoke_list(target))
