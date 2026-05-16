"""Specialist sub-loop — one persona, focused tool subset, one trigger.

The specialist runs a small tool-using LLM loop with the provider configured
for the agent. It loads its skill markdown via the MCP get_skill tool the
first time it runs, then iterates: model picks a tool → MCP executes → result
is fed back. Loop ends when the model produces a final text reply or hits
max_steps.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
from dataclasses import dataclass
from typing import Any

from .mcp_client import MCPClient

log = logging.getLogger("pi_agent.specialist")

# Each step = one LLM round-trip (which may include one or more tool
# calls). 100 leaves plenty of headroom for a thorough analysis: a typical
# query touches ~6-12 tools and finishes well under 20 steps. The cap
# only fires when the model is stuck in a loop. Override via
# PI_AGENT_MAX_STEPS in Settings.
DEFAULT_MAX_STEPS = 100

# Tools whose call by the model means the run has produced a tangible
# output. If we ever exhaust max_steps but at least one of these fired,
# the run is reported as "completed" rather than "max steps reached".
TERMINAL_TOOLS: frozenset[str] = frozenset({
    "push_insight",
    "submit_query_answer",
    "write_observation",
    "set_corner_reminder",
})

# Specialists that may push insights to the driver. The "responder" persona is
# allowed because /api/analyst/query may carry urgent=true; everyone else is
# limited to write_observation unless their skill body grants more.
TOOL_ALLOW_BY_PERSONA: dict[str, set[str]] = {
    "responder": {
        "get_race_state", "get_brain_snapshot", "get_recent_telemetry",
        "list_laps", "get_lap_traces", "get_lap_delta", "compare_lap_corners",
        "get_corner_brake_history", "get_brake_balance_report", "get_corner_coaching_report",
        "query_sql", "describe_schema",
        "recent_insights", "recent_pi_observations",
        "list_sessions", "get_session_history",
        "get_state_race", "get_state_tires", "get_state_energy",
        "get_state_competitors", "get_state_pace", "get_state_events",
        "get_state_track_position", "get_state_proximity",
        "get_skill", "list_skills",
        "submit_query_answer", "write_observation",
    },
}

# Default allow list for unknown / coaching personas. Includes push_insight +
# reminders so the agent can speak to the driver — but priority is capped
# server-side at PI_AGENT_MAX_PRIORITY.
DEFAULT_ALLOW = {
    "get_race_state", "get_brain_snapshot", "get_recent_telemetry",
    "list_laps", "get_lap_traces", "get_lap_delta", "compare_lap_corners",
    "get_corner_brake_history", "get_brake_balance_report", "get_corner_coaching_report",
    "query_sql", "describe_schema",
    "recent_insights", "recent_pi_observations",
    "list_sessions", "get_session_history",
    "get_state_race", "get_state_tires", "get_state_energy",
    "get_state_competitors", "get_state_pace", "get_state_events",
    "get_state_track_position", "get_state_proximity",
    "get_skill", "list_skills",
    "write_observation", "push_insight",
    "set_corner_reminder", "cancel_corner_reminder",
}


@dataclass
class SpecialistConfig:
    persona: str
    provider: str = "anthropic"
    model: str = ""
    max_steps: int = DEFAULT_MAX_STEPS


class Specialist:
    def __init__(self, client: MCPClient, cfg: SpecialistConfig):
        self.client = client
        self.cfg = cfg

    def allowed_tools(self) -> list[dict[str, Any]]:
        allow = TOOL_ALLOW_BY_PERSONA.get(self.cfg.persona, DEFAULT_ALLOW)
        return [t for t in self.client.tools if t["name"] in allow]

    async def run(self, trigger: dict[str, Any]) -> str:
        provider = self.cfg.provider.lower()
        if provider in ("anthropic", "claude"):
            return await self._run_anthropic(trigger)
        if provider == "gemini":
            return await self._run_gemini(trigger)
        if provider == "openai":
            return await self._run_openai(trigger)
        return f"unsupported provider: {self.cfg.provider}"

    # ── Anthropic ────────────────────────────────────────────────────────

    async def _run_anthropic(self, trigger: dict[str, Any]) -> str:
        try:
            from anthropic import Anthropic
        except ImportError:
            return "anthropic SDK not installed"

        client = Anthropic()
        model = self.cfg.model or "claude-sonnet-4-5-20250929"
        tools = [
            {
                "name": t["name"],
                "description": t["description"],
                "input_schema": t["input_schema"],
            }
            for t in self.allowed_tools()
        ]
        system_prompt = self._system_prompt()
        user_prompt = self._user_prompt(trigger)
        messages: list[dict[str, Any]] = [
            {"role": "user", "content": user_prompt},
        ]

        last_text = ""
        terminal_fired = False
        for step in range(self.cfg.max_steps):
            resp = await asyncio.to_thread(
                client.messages.create,
                model=model,
                max_tokens=2000,
                system=system_prompt,
                tools=tools,
                messages=messages,
            )
            tool_uses = []
            text_parts = []
            for block in resp.content:
                if getattr(block, "type", None) == "tool_use":
                    tool_uses.append(block)
                elif getattr(block, "type", None) == "text":
                    text_parts.append(block.text)
            if text_parts:
                last_text = "\n".join(text_parts).strip()
            if not tool_uses:
                return last_text or "(no answer)"
            # Append the assistant's tool calls.
            messages.append(
                {
                    "role": "assistant",
                    "content": [b.model_dump() for b in resp.content],
                }
            )
            tool_results = []
            for tu in tool_uses:
                if tu.name in TERMINAL_TOOLS:
                    terminal_fired = True
                try:
                    out = await self.client.call(tu.name, dict(tu.input or {}))
                except Exception as e:
                    out = f"tool error: {e}"
                tool_results.append(
                    {
                        "type": "tool_result",
                        "tool_use_id": tu.id,
                        "content": out[:8000],
                    }
                )
            messages.append({"role": "user", "content": tool_results})
        return self._budget_exhausted_message(last_text, terminal_fired)

    # ── Gemini ───────────────────────────────────────────────────────────

    async def _run_gemini(self, trigger: dict[str, Any]) -> str:
        try:
            from google import genai
            from google.genai import types
        except ImportError:
            return "google-genai SDK not installed"

        client = genai.Client()
        model = self.cfg.model or "gemini-2.5-flash"
        tools = [
            types.Tool(
                function_declarations=[
                    types.FunctionDeclaration(
                        name=t["name"],
                        description=t["description"][:1024],
                        parameters=t["input_schema"],
                    )
                    for t in self.allowed_tools()
                ]
            )
        ]
        system_prompt = self._system_prompt()
        user_prompt = self._user_prompt(trigger)
        contents: list[Any] = [user_prompt]
        last_text = ""
        terminal_fired = False
        for step in range(self.cfg.max_steps):
            resp = await asyncio.to_thread(
                client.models.generate_content,
                model=model,
                contents=contents,
                config=types.GenerateContentConfig(
                    system_instruction=system_prompt,
                    tools=tools,
                    temperature=0.4,
                ),
            )
            calls = []
            for cand in (resp.candidates or []):
                for part in (cand.content.parts if cand.content else []) or []:
                    fc = getattr(part, "function_call", None)
                    if fc and fc.name:
                        calls.append(fc)
                    elif getattr(part, "text", None):
                        last_text = (part.text or "").strip()
            if not calls:
                return last_text or "(no answer)"
            contents.append(resp.candidates[0].content)
            for call in calls:
                if call.name in TERMINAL_TOOLS:
                    terminal_fired = True
                args = dict(call.args or {})
                try:
                    out = await self.client.call(call.name, args)
                except Exception as e:
                    out = f"tool error: {e}"
                contents.append(
                    types.Content(
                        role="user",
                        parts=[
                            types.Part.from_function_response(
                                name=call.name,
                                response={"result": out[:8000]},
                            )
                        ],
                    )
                )
        return self._budget_exhausted_message(last_text, terminal_fired)

    async def _run_openai(self, trigger: dict[str, Any]) -> str:
        try:
            from openai import OpenAI
        except ImportError:
            return "openai SDK not installed (pip install openai)"

        client = OpenAI()
        # gpt-4o-mini is the conservative default — works with chat.completions
        # tool calling out of the box. Codex / reasoning models (gpt-5-codex,
        # o-series) can be selected explicitly via PI_AGENT_MODEL but may have
        # narrower parameter support.
        model = self.cfg.model or "gpt-4o-mini"
        tools = [
            {
                "type": "function",
                "function": {
                    "name": t["name"],
                    "description": (t["description"] or "")[:1024],
                    "parameters": t["input_schema"] or {"type": "object"},
                },
            }
            for t in self.allowed_tools()
        ]
        system_prompt = self._system_prompt()
        user_prompt = self._user_prompt(trigger)
        messages: list[dict[str, Any]] = [
            {"role": "system", "content": system_prompt},
            {"role": "user", "content": user_prompt},
        ]
        last_text = ""
        terminal_fired = False
        for _step in range(self.cfg.max_steps):
            try:
                resp = await asyncio.to_thread(
                    client.chat.completions.create,
                    model=model,
                    messages=messages,
                    tools=tools,
                    tool_choice="auto",
                )
            except Exception as e:
                log.warning("openai chat.completions failed: %s", e)
                return f"openai error: {e}"
            msg = resp.choices[0].message
            if msg.content:
                last_text = (msg.content or "").strip()
            tool_calls = msg.tool_calls or []
            if not tool_calls:
                return last_text or "(no answer)"
            # Echo the assistant turn so the next request can attach tool results.
            messages.append(
                {
                    "role": "assistant",
                    "content": msg.content or "",
                    "tool_calls": [
                        {
                            "id": tc.id,
                            "type": "function",
                            "function": {
                                "name": tc.function.name,
                                "arguments": tc.function.arguments or "{}",
                            },
                        }
                        for tc in tool_calls
                    ],
                }
            )
            for tc in tool_calls:
                if tc.function.name in TERMINAL_TOOLS:
                    terminal_fired = True
                try:
                    args = json.loads(tc.function.arguments or "{}")
                except json.JSONDecodeError:
                    args = {}
                try:
                    out = await self.client.call(tc.function.name, args)
                except Exception as e:
                    out = f"tool error: {e}"
                messages.append(
                    {
                        "role": "tool",
                        "tool_call_id": tc.id,
                        "content": out[:8000],
                    }
                )
        return self._budget_exhausted_message(last_text, terminal_fired)

    def _budget_exhausted_message(self, last_text: str, terminal_fired: bool) -> str:
        # Three outcomes when the step loop falls off the end without the
        # model producing a final text-only turn:
        #   1. Model already did its job (called push_insight /
        #      write_observation / submit_query_answer) — report
        #      "completed" honestly.
        #   2. Model produced a text turn at some point and we have its
        #      last text — surface it.
        #   3. Model was stuck in a tool-call loop — only then is "max
        #      steps reached" the truthful signal.
        if terminal_fired:
            return last_text or "(completed: terminal action taken, no closing text)"
        if last_text:
            return last_text
        return f"(max steps reached after {self.cfg.max_steps} turns — no terminal action)"

    # ── Prompt construction ──────────────────────────────────────────────

    def _system_prompt(self) -> str:
        # Kept short on purpose. The heavy guidance lives in the per-persona
        # skill; the model loads it via get_skill on its first turn. The
        # historical-access reminder is duplicated here (NOT only in the
        # skill) because models often answer without loading the skill, and
        # the most common failure mode is the model giving up with "I don't
        # have access to past sessions" when in fact it has full DuckDB read
        # access and a transcript reader.
        return (
            "You are part of a sandboxed F1 race engineering analyst team. "
            f"Your current role is the '{self.cfg.persona}' specialist. "
            "Your ONLY way to interact with the system is via the MCP tools. "
            "Always start by calling get_skill with name='%s' to read your "
            "playbook (heuristics, SQL snippets, push criteria). Then gather "
            "the smallest amount of data needed to answer, and finish by "
            "writing an observation, pushing an insight, or — if you were "
            "dispatched for a query trigger — calling submit_query_answer with "
            "the job_id you were given. NEVER push duplicate insights — call "
            "recent_insights and recent_pi_observations first. Keep messages "
            "to the driver to ≤2 short sentences. "
            "\n\nYOU HAVE FULL HISTORICAL ACCESS — do not refuse historical "
            "questions. DuckDB is append-only across every F1 session ever "
            "recorded. The right order for any 'past session at <track>' / "
            "'last race' question is:\n"
            "  1. list_sessions → returns {session_uid, track_name, "
            "session_type_name, first_seen, last_seen, best_lap_ms, "
            "final_position, player_car_index} for the last ~100 sessions. "
            "Pick the matching session_uid (e.g. track_name == 'Melbourne').\n"
            "  2. For telemetry / laps from that session: query_sql against "
            "telemetry_hifreq (has session_uid) or use list_laps?session_uid=… "
            "/ get_lap_traces. Most other tables (session_data, lap_data, "
            "car_status, car_damage) only carry a timestamp — bound them by "
            "the first_seen / last_seen range from step 1.\n"
            "  3. For engineer↔driver dialogue from that session: "
            "get_session_history(scope=<session_uid>) — kinds includes "
            "engineer_speech, analyst_answer, insight_pushed, user_utterance.\n"
            "Never reply 'I cannot access historical data' or 'my tools "
            "provide only real-time analysis' — that is factually wrong; try "
            "list_sessions first."
            "\n\nSTOP CONDITION: once you have called a terminal tool "
            "(push_insight, write_observation, submit_query_answer, or "
            "set_corner_reminder), produce a short text-only reply on your "
            "next turn (a one-line summary of what you did) and DO NOT "
            "call any more tools. Continuing to call tools after the work "
            "is done wastes the step budget and risks hitting the cap." % self.cfg.persona
        )

    def _user_prompt(self, trigger: dict[str, Any]) -> str:
        kind = trigger.get("kind", "")
        if kind == "query":
            return (
                f"A query trigger arrived. job_id={trigger.get('job_id')}, "
                f"context_topic={trigger.get('context_topic')}, urgent={trigger.get('urgent')}.\n"
                f"Driver question:\n  {trigger.get('question')}\n\n"
                "Answer concisely (≤3 sentences) and call submit_query_answer "
                "with that job_id when done."
            )
        if kind == "lap_complete":
            return (
                f"A lap just completed (lap {trigger.get('lap')}). "
                "Review pace, sectors, and tire/brake state. Decide whether "
                "anything is worth telling the driver — only push_insight when "
                "you have a non-obvious actionable fact."
            )
        if kind == "significant_event":
            return (
                f"Rule engine fired a significant event: "
                f"code={trigger.get('event_code')!r}, "
                f"detail={trigger.get('event_detail')!r}, "
                f"meta={json.dumps(trigger.get('meta') or {})}.\n"
                "Decide whether further analysis is needed and act on it."
            )
        return f"Trigger payload: {json.dumps(trigger)}"
