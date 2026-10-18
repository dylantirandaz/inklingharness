"""Host-side checks for complete recorded route evidence."""

import json
from pathlib import Path
import tempfile
import unittest

from harbor.models.agent.context import AgentContext

from evals.harbor_wire import MODEL, PROFILES, audit_wire_providers, populate_wire_context, profile_for


def _routing(provider: str, model: str, requested: str = MODEL) -> dict[str, object]:
    return {
        "requested": requested,
        "endpoints": {"available": [{"provider": provider, "model": model, "selected": True}]},
    }


def _write_wire(logs: Path, routing: dict[str, object] | None, terminated: bool) -> None:
    wire = logs / "wire"
    record = wire / "0001"
    record.mkdir(parents=True)
    request = {
        "model": MODEL,
        "max_tokens": 16384,
        "output_config": {"effort": "high"},
        "provider": {"order": ["deepinfra/fp8"], "allow_fallbacks": False},
        "messages": [{"role": "user", "content": "Check the release date."}],
    }
    for name in ("incoming.json", "0001.request.json"):
        (record / name).write_text(json.dumps(request))
    (wire / "run.json").write_text(json.dumps({"requests": 1}))
    (record / "metadata.json").write_text(json.dumps({
        "path": "/api/v1/messages", "method": "POST", "status_code": 200,
        "content_type": "text/event-stream", "content_encoding": "identity",
    }))
    stop: dict[str, object] = {"type": "message_stop"}
    if routing is not None:
        stop["openrouter_metadata"] = routing
    events = [
        {"type": "message_start", "message": {
            "id": "recorded-server-search", "model": "unknown", "provider": None,
            "usage": {"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": None},
        }},
        {"type": "message_delta", "usage": {
            "input_tokens": 3451, "cache_read_input_tokens": 512, "output_tokens": 595,
        }},
        stop,
    ]
    stream = "".join("data: " + json.dumps(event) + "\n\n" for event in events)
    (record / "0001.response.sse").write_text(stream if terminated else stream.rstrip("\n"))


def _write_profiled_wire(logs: Path, profile_model: str, recorded: dict[str, object], routing: dict[str, object]) -> None:
    """Write a Messages record made under another model's recorded profile."""
    profile = PROFILES[profile_model]
    _write_wire(logs, routing, True)
    wire = logs / "wire"
    request = {
        "model": profile.model,
        "max_tokens": profile.max_tokens,
        "output_config": {"effort": profile.effort},
        "provider": profile.route(),
        "messages": [{"role": "user", "content": "Check the release date."}],
    }
    for name in ("incoming.json", "0001.request.json"):
        (wire / "0001" / name).write_text(json.dumps(request))
    (wire / "profile.json").write_text(json.dumps(recorded))


class RecordedProfileTests(unittest.TestCase):
    def test_astra_profile_is_audited_against_its_own_route(self) -> None:
        astra = PROFILES["openai/gpt-6-astra"]
        with tempfile.TemporaryDirectory() as directory:
            logs = Path(directory)
            _write_profiled_wire(logs, astra.model, astra.recorded(), _routing("OpenAI", astra.backend_model, astra.model))
            context = AgentContext()
            populate_wire_context(logs, context)
            self.assertEqual(context.n_output_tokens, 595)
            assert context.metadata is not None
            self.assertEqual(context.metadata["profile"]["provider"], "openai")
            audit_wire_providers(logs)
            audit = json.loads((logs / "wire" / "provider-audit.json").read_text())
            self.assertEqual(audit["backend_model"], "openai/gpt-6-astra-20260903")

    def test_astra_record_fails_under_the_wrong_route_or_profile(self) -> None:
        astra = PROFILES["openai/gpt-6-astra"]
        cases = {
            "inkling_backend": (astra.recorded(), _routing("DeepInfra", "thinkingmachines/inkling-small-20260730", astra.model)),
            "azure_route": (astra.recorded(), _routing("Azure", astra.backend_model, astra.model)),
            "changed_cap": ({**astra.recorded(), "max_tokens": 16384}, _routing("OpenAI", astra.backend_model, astra.model)),
            "unknown_model": ({**astra.recorded(), "model": "openai/unknown"}, _routing("OpenAI", astra.backend_model, astra.model)),
        }
        with tempfile.TemporaryDirectory() as directory:
            for name, (recorded, routing) in cases.items():
                with self.subTest(case=name):
                    logs = Path(directory) / name
                    _write_profiled_wire(logs, astra.model, recorded, routing)
                    context = AgentContext()
                    with self.assertRaises(ValueError):
                        populate_wire_context(logs, context)
                    self.assertIsNone(context.n_input_tokens)

    def test_effort_override_is_part_of_the_recorded_profile(self) -> None:
        high = profile_for("openai/gpt-6-astra", "high")
        self.assertEqual(high.recorded()["effort"], "high")
        self.assertIn("high", high.wire_flags())
        with self.assertRaises(ValueError):
            profile_for("openai/unknown")


class MessagesRouteTests(unittest.TestCase):
    def test_missing_wire_logs_do_not_claim_usage_or_route(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            logs = Path(directory)
            context = AgentContext()
            populate_wire_context(logs, context)
            self.assertFalse(context.is_empty())
            self.assertIsNone(context.n_input_tokens)
            self.assertIsNone(context.n_cache_tokens)
            self.assertIsNone(context.n_output_tokens)
            assert context.metadata is not None
            self.assertIs(context.metadata["all_recorded_usage_complete"], False)
            self.assertIsNone(context.metadata["process"])
            with self.assertRaises(ValueError):
                audit_wire_providers(logs)

    def test_server_search_identity_is_checked_at_message_stop(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            logs = Path(directory)
            _write_wire(logs, _routing("DeepInfra", "thinkingmachines/inkling-small-20260730"), True)
            context = AgentContext()
            populate_wire_context(logs, context)
            self.assertEqual(context.n_input_tokens, 3963)
            self.assertEqual(context.n_cache_tokens, 512)
            self.assertEqual(context.n_output_tokens, 595)
            assert context.metadata is not None
            self.assertEqual(context.metadata["provider_verified_records"], 1)
            self.assertIs(context.metadata["all_recorded_usage_complete"], True)
            audit_wire_providers(logs)

    def test_token_count_failure_does_not_change_inference_usage(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            logs = Path(directory)
            _write_wire(logs, _routing("DeepInfra", "thinkingmachines/inkling-small-20260730"), True)
            wire = logs / "wire"
            count_record = wire / "0002"
            count_record.mkdir()
            request = {"model": MODEL, "messages": [{"role": "user", "content": "Count this input."}]}
            for name in ("incoming.json", "0001.request.json"):
                (count_record / name).write_text(json.dumps(request))
            (count_record / "metadata.json").write_text(json.dumps({
                "path": "/api/v1/messages/count_tokens", "method": "POST",
                "status_code": 404, "content_type": "application/json",
                "content_encoding": "identity",
            }))
            (count_record / "0001.response.sse").write_text(json.dumps({"error": {"message": "Not found"}}))
            (wire / "run.json").write_text(json.dumps({"requests": 2}))
            context = AgentContext()
            populate_wire_context(logs, context)
            self.assertEqual(
                (context.n_input_tokens, context.n_cache_tokens, context.n_output_tokens),
                (3963, 512, 595),
            )
            assert context.metadata is not None
            self.assertEqual(context.metadata["inference_records"], 1)
            self.assertEqual(context.metadata["token_count_records"], 1)
            self.assertIs(context.metadata["all_recorded_usage_complete"], True)
            audit_wire_providers(logs)

    def test_invalid_route_retains_failure_without_fabricated_usage(self) -> None:
        valid = _routing("DeepInfra", "thinkingmachines/inkling-small-20260730")
        cases: list[tuple[str, dict[str, object] | None]] = [
            ("absent", None),
            ("wrong_provider", _routing("DifferentProvider", "thinkingmachines/inkling-small-20260730")),
            ("wrong_model", _routing("DeepInfra", "different/model")),
            ("wrong_request", {**valid, "requested": "different/model"}),
            ("unselected", {"requested": MODEL, "endpoints": {"available": []}}),
            ("wrong_attempt", {**valid, "attempts": [{"provider": "DifferentProvider", "model": "thinkingmachines/inkling-small-20260730", "status": 200}]}),
        ]
        with tempfile.TemporaryDirectory() as directory:
            for name, routing in cases:
                with self.subTest(route=name):
                    logs = Path(directory) / name
                    _write_wire(logs, routing, True)
                    context = AgentContext()
                    with self.assertRaises(ValueError):
                        populate_wire_context(logs, context)
                    self.assertFalse(context.is_empty())
                    self.assertIsNone(context.n_input_tokens)
                    self.assertIsNone(context.n_cache_tokens)
                    self.assertIsNone(context.n_output_tokens)
                    assert context.metadata is not None
                    self.assertIs(context.metadata["all_recorded_usage_complete"], False)

    def test_unterminated_route_event_does_not_verify_the_response(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            logs = Path(directory)
            _write_wire(logs, _routing("DeepInfra", "thinkingmachines/inkling-small-20260730"), False)
            context = AgentContext()
            populate_wire_context(logs, context)
            self.assertIsNone(context.n_input_tokens)
            self.assertIsNone(context.n_cache_tokens)
            self.assertIsNone(context.n_output_tokens)
            assert context.metadata is not None
            self.assertEqual(context.metadata["provider_verified_records"], 0)
            self.assertIs(context.metadata["all_recorded_usage_complete"], False)
            with self.assertRaises(ValueError):
                audit_wire_providers(logs)


def _write_responses_wire(logs: Path, routing: dict[str, object]) -> None:
    wire = logs / "wire"
    record = wire / "0001"
    record.mkdir(parents=True)
    request = {
        "model": MODEL, "input": "Check the release date.", "max_output_tokens": 16384,
        "reasoning": {"effort": "high"},
        "provider": {"order": ["deepinfra/fp8"], "allow_fallbacks": False},
    }
    for name in ("incoming.json", "0001.request.json"):
        (record / name).write_text(json.dumps(request))
    (wire / "run.json").write_text(json.dumps({"requests": 1}))
    (record / "metadata.json").write_text(json.dumps({
        "path": "/api/v1/responses", "method": "POST", "status_code": 200,
        "content_type": "text/event-stream", "content_encoding": "identity",
    }))
    response = {
        "id": "recorded-response", "model": MODEL, "openrouter_metadata": routing,
        "usage": {"input_tokens": 150, "input_tokens_details": {"cached_tokens": 40}, "output_tokens": 20},
    }
    event = {"type": "response.completed", "response": response}
    (record / "0001.response.sse").write_text("data: " + json.dumps(event) + "\n\n")


class ResponsesRouteTests(unittest.TestCase):
    def test_terminal_route_does_not_need_a_later_generation_record(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            logs = Path(directory)
            _write_responses_wire(logs, _routing("DeepInfra", "thinkingmachines/inkling-small-20260730"))
            context = AgentContext()
            populate_wire_context(logs, context)
            self.assertEqual(context.n_input_tokens, 150)
            self.assertEqual(context.n_cache_tokens, 40)
            self.assertEqual(context.n_output_tokens, 20)
            assert context.metadata is not None
            self.assertEqual(context.metadata["provider_verified_records"], 1)
            self.assertEqual(context.metadata["pending_provider_verification"], [])
            self.assertIs(context.metadata["all_recorded_usage_complete"], True)
            audit_wire_providers(logs)

    def test_failed_response_without_identity_is_a_failed_record_not_a_crash(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            logs = Path(directory)
            _write_responses_wire(logs, _routing("DeepInfra", "thinkingmachines/inkling-small-20260730"))
            record = logs / "wire" / "0001"
            failed = {"type": "response.failed", "response": {"status": "failed", "error": {"code": "server_error"}}}
            (record / "0001.response.sse").write_text("data: " + json.dumps(failed) + "\n\n")
            context = AgentContext()
            populate_wire_context(logs, context)
            self.assertIsNone(context.n_input_tokens)
            assert context.metadata is not None
            self.assertEqual(context.metadata["failed_or_incomplete_records"], 1)
            self.assertIs(context.metadata["all_recorded_usage_complete"], False)

    def test_wrong_model_in_failed_response_still_raises(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            logs = Path(directory)
            _write_responses_wire(logs, _routing("DeepInfra", "thinkingmachines/inkling-small-20260730"))
            record = logs / "wire" / "0001"
            failed = {"type": "response.failed", "response": {"id": "x", "model": "other/model", "status": "failed"}}
            (record / "0001.response.sse").write_text("data: " + json.dumps(failed) + "\n\n")
            with self.assertRaises(ValueError):
                populate_wire_context(logs, AgentContext())

    def test_wrong_terminal_provider_does_not_enter_the_pending_audit(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            logs = Path(directory)
            _write_responses_wire(logs, _routing("DifferentProvider", "thinkingmachines/inkling-small-20260730"))
            context = AgentContext()
            with self.assertRaises(ValueError):
                populate_wire_context(logs, context)
            self.assertIsNone(context.n_input_tokens)
            self.assertIsNone(context.n_cache_tokens)
            self.assertIsNone(context.n_output_tokens)
            assert context.metadata is not None
            self.assertIs(context.metadata["all_recorded_usage_complete"], False)


def _write_prime_wire(logs: Path, *, forwarded_route: bool = False, inference_id: str | None = "req_1", returned_model: str | None = None) -> None:
    profile = PROFILES["prime/deepseek-v4.1-flash"]
    wire = logs / "wire"
    record = wire / "0001"
    record.mkdir(parents=True)
    incoming = {
        "model": profile.model, "max_tokens": profile.max_tokens,
        "output_config": {"effort": profile.effort}, "provider": profile.route(),
        "messages": [{"role": "user", "content": "Check the release date."}],
    }
    forwarded = dict(incoming)
    if not forwarded_route:
        forwarded.pop("provider")
    (record / "incoming.json").write_text(json.dumps(incoming))
    (record / "0001.request.json").write_text(json.dumps(forwarded))
    (wire / "profile.json").write_text(json.dumps(profile.recorded()))
    (wire / "run.json").write_text(json.dumps({"requests": 1}))
    metadata: dict[str, object] = {
        "path": "/api/v1/messages", "method": "POST", "status_code": 200,
        "content_type": "text/event-stream; charset=utf-8", "content_encoding": "",
    }
    if inference_id is not None:
        metadata["upstream_request_id"] = inference_id
    (record / "metadata.json").write_text(json.dumps(metadata))
    events = [
        {"type": "message_start", "message": {"id": "msg_1", "model": returned_model or profile.model, "usage": {"input_tokens": 7738, "output_tokens": 1}}},
        {"type": "message_delta", "usage": {"input_tokens": 7738, "output_tokens": 32, "cache_read_input_tokens": 7552}},
        {"type": "message_stop"},
    ]
    (record / "0001.response.sse").write_text("".join("data: " + json.dumps(event) + "\n\n" for event in events))


class PrimeRouteTests(unittest.TestCase):
    def test_prime_record_is_verified_without_double_counting_cache(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            logs = Path(directory)
            _write_prime_wire(logs)
            context = AgentContext()
            populate_wire_context(logs, context)
            # Prime's input_tokens already includes the cached prefix.
            self.assertEqual(context.n_input_tokens, 7738)
            self.assertEqual(context.n_cache_tokens, 7552)
            self.assertEqual(context.n_output_tokens, 32)
            assert context.metadata is not None
            self.assertEqual(context.metadata["provider_verified_records"], 1)
            self.assertEqual(context.metadata["usage_source"], "raw Prime Intellect wire responses")
            audit_wire_providers(logs)
            self.assertIn("-upstream", PROFILES["prime/deepseek-v4.1-flash"].wire_flags())

    def test_prime_record_fails_without_evidence_or_with_a_forwarded_route(self) -> None:
        cases = {
            "route_forwarded": {"forwarded_route": True},
            "no_inference_id": {"inference_id": None},
            "wrong_model": {"returned_model": "deepseek/deepseek-v4-flash"},
        }
        with tempfile.TemporaryDirectory() as directory:
            for name, options in cases.items():
                with self.subTest(case=name):
                    logs = Path(directory) / name
                    _write_prime_wire(logs, **options)
                    context = AgentContext()
                    with self.assertRaises(ValueError):
                        populate_wire_context(logs, context)
                    self.assertIsNone(context.n_input_tokens)


if __name__ == "__main__":
    unittest.main()
