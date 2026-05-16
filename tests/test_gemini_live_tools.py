"""Unit tests for gemini_live_service.call_tool dispatcher.

The dispatcher routes Gemini Live tool calls to the Go telemetry-core HTTP
API. We mock urllib so the tests run without a running server or Gemini SDK
auth. Audio loop and Live session aren't exercised here.
"""

from __future__ import annotations

import importlib
import io
import json
import sys
import unittest
from typing import Optional
from unittest.mock import patch


def _load_service():
    """Import gemini_live_service after stubbing pyaudio + google.genai so
    the import doesn't crash on machines that lack PortAudio or API keys."""
    if "gemini_live_service" in sys.modules:
        del sys.modules["gemini_live_service"]

    fake_pyaudio = type(sys)("pyaudio")
    fake_pyaudio.paInt16 = 8

    class _PA:
        def get_device_count(self):
            return 0

        def terminate(self):
            pass

    fake_pyaudio.PyAudio = _PA  # type: ignore[attr-defined]
    sys.modules["pyaudio"] = fake_pyaudio

    fake_genai = type(sys)("google.genai")

    class _Client:
        def __init__(self, *_, **__):
            pass

    fake_genai.Client = _Client  # type: ignore[attr-defined]

    fake_types = type(sys)("google.genai.types")

    class _FR:
        def __init__(self, **kw):
            self.kw = kw

    fake_types.FunctionResponse = _FR  # type: ignore[attr-defined]

    google_pkg = type(sys)("google")
    google_pkg.genai = fake_genai  # type: ignore[attr-defined]
    sys.modules.setdefault("google", google_pkg)
    sys.modules["google.genai"] = fake_genai
    sys.modules["google.genai.types"] = fake_types

    return importlib.import_module("gemini_live_service")


class _FakeResponse:
    def __init__(self, body: str, status: int = 200):
        self._body = body.encode("utf-8")
        self.status = status

    def read(self):
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return False


class _Recorder:
    """Replaces urllib.request.urlopen. Records every request and replays
    scripted responses keyed by (method, path). script_sequence supports
    polling tests where the same endpoint returns evolving payloads."""

    def __init__(self):
        self.calls: list[dict] = []
        self.scripts: dict[tuple[str, str], _FakeResponse] = {}
        self.sequences: dict[tuple[str, str], list[_FakeResponse]] = {}

    def script(self, method: str, path: str, body: str, status: int = 200):
        self.scripts[(method.upper(), path)] = _FakeResponse(body, status)

    def script_sequence(self, method: str, path: str, bodies: list[str], status: int = 200):
        self.sequences[(method.upper(), path)] = [
            _FakeResponse(b, status) for b in bodies
        ]

    def __call__(self, req, timeout=None):
        method = req.get_method()
        url = req.full_url
        # Strip the host prefix so callers script only the path.
        path = url.split("/", 3)[-1]
        path = "/" + path if not path.startswith("/") else path
        body_bytes = None
        if hasattr(req, "data") and req.data is not None:
            body_bytes = req.data
        self.calls.append(
            {"method": method, "path": path, "body": body_bytes, "timeout": timeout}
        )
        key = (method.upper(), path)
        if key in self.sequences and self.sequences[key]:
            seq = self.sequences[key]
            if len(seq) == 1:
                return seq[0]
            return seq.pop(0)
        if key in self.scripts:
            return self.scripts[key]
        return _FakeResponse(json.dumps({"ok": True}))


class CallToolTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.svc = _load_service()

    def setUp(self):
        self.rec = _Recorder()
        self.patcher = patch.object(self.svc.urllib.request, "urlopen", self.rec)
        self.patcher.start()

    def tearDown(self):
        self.patcher.stop()

    def test_get_race_state_calls_telemetry_endpoint(self):
        self.rec.script(
            "GET",
            "/api/telemetry/latest",
            json.dumps({"position": 3, "lap": 18}),
        )
        result = self.svc.call_tool("get_race_state", {})
        self.assertIn("result", result)
        self.assertIn("position", result["result"])
        self.assertEqual(self.rec.calls[0]["method"], "GET")
        self.assertEqual(self.rec.calls[0]["path"], "/api/telemetry/latest")

    def test_query_brain_returns_truncated_markdown(self):
        big_md = "## live\n" + "x" * (self.svc.BRAIN_MAX_CHARS + 200)
        self.rec.script("GET", "/api/brain/snapshot", big_md)
        result = self.svc.call_tool("query_brain", {})
        self.assertTrue(len(result["result"]) <= self.svc.BRAIN_MAX_CHARS + 64)
        self.assertIn("truncated", result["result"])

    def test_ask_data_analyst_submits_then_polls_brain_for_answer(self):
        # 1. Submit returns 202 with a job_id.
        self.rec.script(
            "POST",
            "/api/analyst/query",
            json.dumps({"job_id": "anq_abc123", "status": "queued", "eta_seconds": 10}),
        )
        # 2. First brain poll: empty (job not finished yet).
        # 3. Second brain poll: observation under analyst.tire_strategy.
        self.rec.script_sequence(
            "GET",
            "/api/brain/snapshot?format=json",
            [
                json.dumps({"observations": {}}),
                json.dumps(
                    {
                        "observations": {
                            "analyst.tire_strategy": [
                                {
                                    "job_id": "anq_abc123",
                                    "summary": "Eight laps left in the softs",
                                    "agent": "analyst",
                                }
                            ]
                        }
                    }
                ),
            ],
        )
        # Pending-jobs may also be polled — keep returning the job in flight
        # until the brain reports it.
        self.rec.script("GET", "/api/analyst/jobs", json.dumps([{"job_id": "anq_abc123"}]))

        # Cap poll interval so the test runs fast.
        self.svc.ANALYST_TIMEOUT = 5.0
        with patch.object(self.svc.time, "sleep", lambda _s: None):
            result = self.svc.call_tool(
                "ask_data_analyst",
                {"question": "how are my tires?", "context_topic": "tire_strategy"},
            )

        self.assertEqual(result["result"], "Eight laps left in the softs")
        self.assertEqual(result["job_id"], "anq_abc123")
        self.assertEqual(result["context_topic"], "tire_strategy")

        # First call must be the submit POST with full payload.
        submit = self.rec.calls[0]
        self.assertEqual(submit["method"], "POST")
        self.assertEqual(submit["path"], "/api/analyst/query")
        body = json.loads(submit["body"].decode())
        self.assertEqual(
            body,
            {"question": "how are my tires?", "context_topic": "tire_strategy", "urgent": False},
        )

    def test_ask_data_analyst_rejects_empty_question(self):
        result = self.svc.call_tool("ask_data_analyst", {"question": "  "})
        self.assertIn("error", result)
        self.assertEqual(self.rec.calls, [])

    def test_ask_data_analyst_propagates_submit_error(self):
        self.rec.script(
            "POST",
            "/api/analyst/query",
            json.dumps({"error": "analyst not initialised"}),
        )
        result = self.svc.call_tool("ask_data_analyst", {"question": "x"})
        self.assertIn("error", result)
        self.assertIn("analyst not initialised", result["error"])

    def test_ask_data_analyst_times_out_when_brain_never_lands(self):
        self.rec.script(
            "POST",
            "/api/analyst/query",
            json.dumps({"job_id": "anq_slow", "status": "queued"}),
        )
        # Brain never produces the observation.
        self.rec.script("GET", "/api/brain/snapshot?format=json", json.dumps({"observations": {}}))
        # Pending-jobs always shows the job still running.
        self.rec.script("GET", "/api/analyst/jobs", json.dumps([{"job_id": "anq_slow"}]))

        self.svc.ANALYST_TIMEOUT = 0.1  # immediate
        with patch.object(self.svc.time, "sleep", lambda _s: None):
            result = self.svc.call_tool("ask_data_analyst", {"question": "deep one"})
        self.assertIn("error", result)
        self.assertIn("timed out", result["error"])

    def test_ask_data_analyst_passes_urgent_flag(self):
        self.rec.script(
            "POST",
            "/api/analyst/query",
            json.dumps({"job_id": "anq_urg", "status": "queued"}),
        )
        self.rec.script(
            "GET",
            "/api/brain/snapshot?format=json",
            json.dumps(
                {
                    "observations": {
                        "analyst.general": [
                            {"job_id": "anq_urg", "summary": "box now", "urgent": True}
                        ]
                    }
                }
            ),
        )
        self.rec.script("GET", "/api/analyst/jobs", json.dumps([]))

        self.svc.ANALYST_TIMEOUT = 5.0
        with patch.object(self.svc.time, "sleep", lambda _s: None):
            result = self.svc.call_tool(
                "ask_data_analyst", {"question": "act now?", "urgent": True}
            )

        self.assertEqual(result["result"], "box now")
        body = json.loads(self.rec.calls[0]["body"].decode())
        self.assertTrue(body["urgent"])

    def test_push_strategy_insight_posts_full_payload(self):
        self.rec.script(
            "POST",
            "/api/strategy",
            json.dumps({"status": "accepted"}),
        )
        result = self.svc.call_tool(
            "push_strategy_insight",
            {"summary": "tire cliff", "recommendation": "box now", "priority": 4},
        )
        self.assertEqual(result["result"], "insight accepted by strategy pipeline")

        body = json.loads(self.rec.calls[0]["body"].decode())
        self.assertEqual(
            body,
            {"summary": "tire cliff", "recommendation": "box now", "criticality": 4},
        )

    def test_push_strategy_insight_requires_summary_and_recommendation(self):
        result = self.svc.call_tool(
            "push_strategy_insight", {"summary": "", "recommendation": "x", "priority": 3}
        )
        self.assertIn("error", result)
        self.assertEqual(self.rec.calls, [])

    def test_unknown_tool_returns_error(self):
        result = self.svc.call_tool("not_a_tool", {})
        self.assertEqual(result, {"error": "unknown tool not_a_tool"})

    def test_upstream_unreachable_is_caught(self):
        import urllib.error

        def boom(*_, **__):
            raise urllib.error.URLError("connection refused")

        with patch.object(self.svc.urllib.request, "urlopen", boom):
            result = self.svc.call_tool("get_race_state", {})
        self.assertIn("error", result)
        self.assertIn("upstream", result["error"])


if __name__ == "__main__":
    unittest.main()
