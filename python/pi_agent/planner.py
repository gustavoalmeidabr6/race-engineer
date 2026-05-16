"""Planner loop — long-polls the MCP trigger queue and dispatches specialists.

The planner is the only persistent loop in the pi agent. It does no analysis
itself; it picks which specialist persona should react to each trigger and
runs that specialist's sub-loop with a focused tool subset.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import secrets
import time
from dataclasses import dataclass
from typing import Any

from .mcp_client import MCPClient, current_persona, current_run_id
from .specialist import Specialist, SpecialistConfig

log = logging.getLogger("pi_agent.planner")


# Specialists are picked by trigger kind and event code. Add new entries here
# as you ship new skills. The "skill" key is loaded lazily via get_skill so
# the planner doesn't carry every playbook in its prompt.
PERSONAS_BY_TRIGGER: dict[str, str] = {
    "lap_complete": "pace",
    "query": "responder",
}

# For TriggerSignificant, the persona is picked by event-code prefix.
PERSONAS_BY_EVENT_CODE_PREFIX: list[tuple[str, str]] = [
    ("Tire", "tires"),
    ("TireWear", "tires"),
    ("TireTemp", "tires"),
    ("Brake", "brakes"),
    ("BrakeTemp", "brakes"),
    ("Fuel", "energy"),
    ("ERS", "energy"),
    ("Damage", "strategy"),
    ("Component", "strategy"),
    ("Rain", "weather"),
    ("SafetyCar", "strategy"),
    ("Position", "strategy"),
]


def pick_persona(trigger: dict[str, Any]) -> str:
    kind = str(trigger.get("kind", ""))
    if kind == "query":
        return "responder"
    if kind == "lap_complete":
        return "pace"
    if kind == "significant_event":
        code = str(trigger.get("event_code", ""))
        for prefix, persona in PERSONAS_BY_EVENT_CODE_PREFIX:
            if code.startswith(prefix):
                return persona
        return "strategy"
    return "strategy"


@dataclass
class PlannerConfig:
    mcp_url: str
    provider: str = "anthropic"
    model: str = ""
    specialist_model: str = ""
    poll_timeout_sec: int = 10
    max_concurrent_runs: int = 4
    max_steps: int = 100


class Planner:
    def __init__(self, cfg: PlannerConfig):
        self.cfg = cfg
        self.client = MCPClient(url=cfg.mcp_url)
        # Bound concurrent specialists so a slow run can't starve the queue.
        # Each pulled trigger spawns one asyncio task that takes a semaphore
        # slot before invoking the LLM; the planner's pull-loop never blocks
        # on a specialist completing.
        self._slots = asyncio.Semaphore(max(1, cfg.max_concurrent_runs))
        self._inflight: set[asyncio.Task[None]] = set()

    async def run(self) -> None:
        # Go core and pi-agent are launched concurrently, so the MCP endpoint
        # may not be bound (or fully serving) when we first dial. Retry with
        # capped exponential backoff. Wrap each attempt in wait_for so a
        # stalled handshake (TCP up but server not yet processing requests)
        # cancels and retries instead of hanging the whole process.
        connect_backoff = 1.0
        outer_task = asyncio.current_task()
        while True:
            try:
                await asyncio.wait_for(self.client.connect(), timeout=8.0)
                break
            except asyncio.CancelledError:
                # Distinguish outer cancellation (process shutdown) from
                # inner cancellation (wait_for timeout / streamable-HTTP
                # task group propagation). Only the former should abort the
                # retry loop.
                if outer_task is not None and outer_task.cancelling() > 0:
                    raise
                log.warning(
                    "MCP connect cancelled mid-handshake — retrying in %.1fs",
                    connect_backoff,
                )
                await asyncio.sleep(connect_backoff)
                connect_backoff = min(connect_backoff * 1.5, 15.0)
            except asyncio.TimeoutError:
                log.warning(
                    "MCP connect timed out after 8s — retrying in %.1fs",
                    connect_backoff,
                )
                await asyncio.sleep(connect_backoff)
                connect_backoff = min(connect_backoff * 1.5, 15.0)
            except BaseException as e:
                log.warning(
                    "MCP connect failed (%s: %s) — retrying in %.1fs",
                    type(e).__name__, e, connect_backoff,
                )
                await asyncio.sleep(connect_backoff)
                connect_backoff = min(connect_backoff * 1.5, 15.0)
        log.info("planner ready (provider=%s model=%s)", self.cfg.provider, self.cfg.model or "<default>")
        backoff = 1.0
        while True:
            try:
                await self._tick()
                backoff = 1.0
            except asyncio.CancelledError:
                raise
            except Exception:
                log.exception("planner tick failed; backing off %.1fs", backoff)
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 30.0)

    async def _tick(self) -> None:
        # Pull the next trigger and spawn a task for it. The pull itself
        # has no run_id; the spawned task picks one up before it touches
        # the specialist.
        raw = await self.client.call(
            "pull_next_trigger", {"timeout_seconds": self.cfg.poll_timeout_sec}
        )
        try:
            trigger = json.loads(raw)
        except json.JSONDecodeError:
            log.warning("non-JSON trigger payload, skipping: %r", raw[:200])
            return
        if trigger.get("kind") in (None, "none"):
            return
        # Spawn the run. Don't await — the planner loops back to pull the
        # next trigger immediately so multiple specialists can be in flight
        # at once (bounded by self._slots).
        task = asyncio.create_task(self._handle_trigger(trigger))
        self._inflight.add(task)
        task.add_done_callback(self._inflight.discard)

    async def _handle_trigger(self, trigger: dict[str, Any]) -> None:
        persona = pick_persona(trigger)
        run_id = self._mint_run_id(trigger)
        # Bind run_id / persona as contextvars for this task. MCPClient.call
        # reads them and injects `_run_id` / `_persona` into every tool-call
        # arg dict, so the Go MCP hook can group activities per run.
        run_tok = current_run_id.set(run_id)
        per_tok = current_persona.set(persona)
        try:
            async with self._slots:
                log.info(
                    "run %s start → persona %s (kind=%s job=%s code=%s lap=%s)",
                    run_id, persona,
                    trigger.get("kind"),
                    trigger.get("job_id", ""),
                    trigger.get("event_code", ""),
                    trigger.get("lap", ""),
                )
                started = time.time()
                spec = Specialist(
                    client=self.client,
                    cfg=SpecialistConfig(
                        persona=persona,
                        provider=self.cfg.provider,
                        model=self.cfg.specialist_model or self.cfg.model,
                        max_steps=self.cfg.max_steps,
                    ),
                )
                try:
                    conclusion = await spec.run(trigger)
                except Exception as e:
                    log.exception("run %s: specialist %s crashed", run_id, persona)
                    conclusion = f"specialist error: {e}"
                duration = time.time() - started
                log.info(
                    "run %s done → persona %s duration=%.1fs",
                    run_id, persona, duration,
                )
                await self._record_thinking(persona, trigger, conclusion, duration)
        finally:
            current_run_id.reset(run_tok)
            current_persona.reset(per_tok)

    @staticmethod
    def _mint_run_id(trigger: dict[str, Any]) -> str:
        # Prefer the queue-assigned job_id when present (query triggers)
        # so the dashboard can correlate analyst question → run row.
        # Falls back to a random token for other trigger kinds.
        jid = str(trigger.get("job_id") or "").strip()
        if jid:
            return "run_" + jid.replace("anq_", "")
        return "run_" + secrets.token_hex(4)

    async def _record_thinking(
        self,
        persona: str,
        trigger: dict[str, Any],
        conclusion: str,
        duration_sec: float,
    ) -> None:
        body = {
            "trigger_kind": trigger.get("kind"),
            "event_code": trigger.get("event_code"),
            "lap": trigger.get("lap"),
            "job_id": trigger.get("job_id"),
            "duration_sec": round(duration_sec, 2),
            "conclusion": conclusion[:1000],
        }
        try:
            await self.client.call(
                "write_observation",
                {
                    "topic": "thinking",
                    "summary": f"{persona}: {conclusion[:140]}",
                    "body": json.dumps(body),
                    "hypothesis": True,
                    "confidence": 0.6,
                },
            )
        except Exception:
            log.exception("failed to write thinking observation")


async def run_forever(cfg: PlannerConfig) -> None:
    planner = Planner(cfg)
    try:
        await planner.run()
    finally:
        await planner.client.close()


def from_env() -> PlannerConfig:
    base = os.environ.get("RACE_API_URL", "http://localhost:8081").rstrip("/")
    path = os.environ.get("PI_AGENT_MCP_PATH", "/mcp")
    timeout = int(os.environ.get("PI_AGENT_TRIGGER_TIMEOUT_SEC", "10") or "10")
    max_runs = int(os.environ.get("PI_AGENT_MAX_CONCURRENT_RUNS", "4") or "4")
    max_steps = int(os.environ.get("PI_AGENT_MAX_STEPS", "100") or "100")
    return PlannerConfig(
        mcp_url=f"{base}{path}",
        provider=os.environ.get("PI_AGENT_PROVIDER", "anthropic"),
        model=os.environ.get("PI_AGENT_MODEL", ""),
        specialist_model=os.environ.get("PI_AGENT_SPECIALIST_MODEL", ""),
        poll_timeout_sec=timeout,
        max_concurrent_runs=max(1, max_runs),
        max_steps=max(1, max_steps),
    )
