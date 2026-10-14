"""Read complete native wire evidence; missing usage is not zero usage."""

import argparse
import asyncio
from collections.abc import Iterator
from dataclasses import asdict, dataclass, replace
import json
import os
from pathlib import Path
import urllib.error
import urllib.parse
import urllib.request

from harbor.agents.installed.base import BaseInstalledAgent
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

MODEL = "thinkingmachines/inkling-small"


@dataclass(frozen=True)
class WireProfile:
    """The fixed inference profile of a run, enforced by harbor-wire.

    provider is the OpenRouter route slug; provider_name and backend_model are
    what OpenRouter reports for that route.
    """

    model: str
    backend_model: str
    provider: str
    provider_name: str
    effort: str
    max_tokens: int
    context_length: int
    # Per-million-token prices on the route, for native cost displays only.
    price_input: float
    price_output: float
    price_cache_read: float
    # "openrouter", or "prime" for Prime Intellect Inference. Harnesses send
    # the same route either way; for Prime, harbor-wire checks and strips it.
    upstream: str = "openrouter"

    def route(self) -> dict[str, object]:
        return {"order": [self.provider], "allow_fallbacks": False}

    def wire_flags(self) -> list[str]:
        flags = [
            "-model", self.model, "-provider", self.provider,
            "-effort", self.effort, "-max-tokens", str(self.max_tokens),
        ]
        if self.upstream != "openrouter":
            flags += ["-upstream", self.upstream]
        return flags

    def recorded(self) -> dict[str, object]:
        recorded: dict[str, object] = {"model": self.model, "provider": self.provider, "effort": self.effort, "max_tokens": self.max_tokens}
        if self.upstream != "openrouter":
            recorded["upstream"] = self.upstream
        return recorded


PROFILES = {
    MODEL: WireProfile(MODEL, "thinkingmachines/inkling-small-20260730", "deepinfra/fp8", "DeepInfra", "high", 16384, 524288, 0.45, 1.2, 0.1),
    # OpenAI's standard tier, priced as OpenAI's own API. The public Codex and
    # Pi Terminal-Bench 4.0 jobs ran this model at xhigh with its full output.
    "openai/gpt-6-astra": WireProfile(
        "openai/gpt-6-astra", "openai/gpt-6-astra-20260903", "openai", "OpenAI", "xhigh", 128000, 1050000, 10.0, 50.0, 1.0
    ),
    # The official Terminal-Bench 4.0 entry for this model is Codex at max
    # effort, so max is the matched setting.
    "openai/gpt-6-luna": WireProfile(
        "openai/gpt-6-luna", "openai/gpt-6-luna-20260922", "openai", "OpenAI", "max", 128000, 1050000, 0.1, 0.5, 0.01
    ),
    # DeepSeek's own Terminal-Bench 4.0 result (31.2%) used maximum effort.
    # Inference goes through OpenRouter pinned to Novita fp8 (no training on
    # prompts, caches repeated prefixes); sandboxes still run on Prime.
    # DeepInfra is cheaper but rejects images inside tool results ("Input
    # should be a valid string"), which harnesses send after reading a
    # screenshot; Novita accepts images there and in user messages.
    "deepseek/deepseek-v4.1-flash": WireProfile(
        "deepseek/deepseek-v4.1-flash", "deepseek/deepseek-v4.1-flash-20260910", "novita/fp8", "Novita", "max", 128000, 1048576, 0.195, 0.78, 0.0039
    ),
    # The same model on Prime Intellect Inference, kept for comparison runs.
    # Prime ignores the effort field on every endpoint.
    "prime/deepseek-v4.1-flash": WireProfile(
        "deepseek/deepseek-v4.1-flash", "deepseek/deepseek-v4.1-flash", "prime", "Prime Intellect", "max", 128000, 1048576, 0.30, 1.20, 0.03, "prime"
    ),
}


def profile_for(model_name: str, effort: str | None = None) -> WireProfile:
    if model_name not in PROFILES:
        raise ValueError(f"No wire profile for model {model_name!r}; known: {sorted(PROFILES)}")
    profile = PROFILES[model_name]
    return profile if effort is None else replace(profile, effort=effort)


def recorded_profile(wire: Path) -> WireProfile:
    """Return the profile harbor-wire enforced. Runs before profiles were
    recorded used the Inkling Small profile."""
    path = wire / "profile.json"
    if not path.exists():
        return PROFILES[MODEL]
    recorded = _read_object(path)
    model = recorded.get("model")
    if not isinstance(model, str):
        raise ValueError(f"Invalid recorded profile in {wire}")
    upstream = recorded.get("upstream")
    if isinstance(upstream, str):
        # Non-OpenRouter profiles are keyed by upstream and model name.
        model = f"{upstream}/{model.split('/', 1)[-1]}"
    profile = profile_for(model, recorded.get("effort") if isinstance(recorded.get("effort"), str) else None)
    if profile.recorded() != recorded:
        raise ValueError(f"Recorded profile {recorded} differs from the known profile {profile.recorded()} in {wire}")
    return profile
_TOKEN_FIELDS = ("input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "output_tokens")


def validate_linux_binary(path: str | Path) -> Path:
    resolved = Path(path).expanduser().resolve(strict=True)
    with resolved.open("rb") as binary:
        header = binary.read(20)
    if header[:6] != b"\x7fELF\x02\x01" or header[18:20] != b"\x3e\x00":
        raise ValueError(f"Expected a Linux amd64 ELF binary: {resolved}")
    return resolved


async def install_wire(agent: BaseInstalledAgent, environment: BaseEnvironment, binary_path: Path) -> None:
    await agent.ensure_system_dependencies(environment, ("git",))
    await environment.upload_file(binary_path, "/installed-agent/harbor-wire")
    await agent.exec_as_root(environment, command="chmod 755 /installed-agent/harbor-wire")


async def exec_wire(
    agent: BaseInstalledAgent,
    environment: BaseEnvironment,
    *,
    command: str,
    env: dict[str, str] | None = None,
) -> None:
    # Modal limits remote exec arguments to 64 KiB. A file keeps the command exact.
    await agent._upload_config_text(
        environment,
        content=command,
        remote_path="/installed-agent/harbor-run.sh",
        filename="harbor-run.sh",
    )
    try:
        await agent.exec_as_agent(
            environment, command="exec /bin/bash /installed-agent/harbor-run.sh", env=env
        )
    except asyncio.CancelledError:
        # Harbor cancels its RPC wait, but Modal leaves the remote process alive.
        stopped = await environment.exec(
            command=(
                "set -eu; if test -r /logs/agent/wire/pid; then "
                "read -r pid < /logs/agent/wire/pid; "
                "case \"$pid\" in ''|*[!0-9]*) exit 2;; esac; "
                'if test -d "/proc/$pid"; then '
                'test "$(readlink "/proc/$pid/exe")" = /installed-agent/harbor-wire; '
                'kill -TERM "$pid"; '
                'while test -d "/proc/$pid"; do sleep 0.05; done; '
                "fi; fi"
            ),
            timeout_sec=10,
        )
        if stopped.return_code != 0:
            raise RuntimeError(f"Cannot stop the native agent after cancellation: {stopped.stderr}")
        raise


def _object(value: object) -> dict[str, object]:
    if not isinstance(value, dict):
        raise ValueError("Expected a JSON object in the wire record")
    return value


def _read_object(path: Path) -> dict[str, object]:
    value: object = json.loads(path.read_bytes())
    return _object(value)


def _events(path: Path) -> Iterator[dict[str, object]]:
    data: list[bytes] = []
    with path.open("rb") as stream:
        for line in stream:
            line = line.rstrip(b"\r\n")
            if line.startswith(b"data:"):
                data.append(line[5:].removeprefix(b" "))
                continue
            if line or not data:
                continue
            payload = b"\n".join(data)
            data.clear()
            if payload != b"[DONE]":
                value: object = json.loads(payload)
                yield _object(value)
    # An unfinished final frame has no complete event to count.


def _count(value: object) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or value < 0:
        raise ValueError("Expected a nonnegative integer token count")
    return value


def _audit_request(directory: Path, endpoint: str, profile: WireProfile) -> None:
    incoming = _read_object(directory / "incoming.json")
    forwarded = _read_object(directory / "0001.request.json")
    if forwarded.get("model") != profile.model or incoming.get("model") != profile.model:
        raise ValueError(f"Wrong model in {directory}")
    expected = dict(incoming)
    if profile.upstream == "prime":
        # harbor-wire forwards no route to Prime.
        if "provider" in forwarded:
            raise ValueError(f"A route was forwarded to Prime in {directory}")
        expected.pop("provider", None)
    else:
        if forwarded.get("provider") != profile.route():
            raise ValueError(f"Wrong provider profile in {directory}")
        expected["provider"] = profile.route()
    if endpoint == "/api/v1/messages":
        if forwarded.get("max_tokens") != profile.max_tokens or _object(forwarded.get("output_config")).get("effort") != profile.effort:
            raise ValueError(f"Wrong Messages profile in {directory}")
        expected["max_tokens"] = profile.max_tokens
        output = dict(_object(incoming["output_config"])) if "output_config" in incoming else {}
        output["effort"] = profile.effort
        expected["output_config"] = output
        for field in ("thinking", "temperature"):
            if field not in forwarded:
                expected.pop(field, None)
    elif endpoint == "/api/v1/responses":
        if forwarded.get("max_output_tokens") != profile.max_tokens or _object(forwarded.get("reasoning")).get("effort") != profile.effort:
            raise ValueError(f"Wrong Responses profile in {directory}")
        expected["max_output_tokens"] = profile.max_tokens
    else:
        raise ValueError(f"Unsupported recorded endpoint {endpoint!r}")
    if forwarded != expected:
        raise ValueError(f"The recorder changed a prompt, tool, history, or other non-profile field in {directory}")


def _generation_metadata(identifier: str, directory: Path, profile: WireProfile) -> dict[str, object]:
    saved = directory / "generation.json"
    if saved.exists():
        body = saved.read_bytes()
    else:
        key = os.environ.get("OPENROUTER_API_KEY")
        if not key:
            raise ValueError("OPENROUTER_API_KEY is required to verify the Responses provider")
        request = urllib.request.Request(
            "https://openrouter.ai/api/v1/generation?" + urllib.parse.urlencode({"id": identifier}),
            headers={"Authorization": f"Bearer {key}"},
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                body = response.read()
        except urllib.error.HTTPError as error:
            (directory / "generation-error.json").write_text(json.dumps({"status": error.code, "body": error.read().decode()}) + "\n")
            raise
    value: object = json.loads(body)
    metadata = _object(_object(value).get("data"))
    if metadata.get("id") != identifier or metadata.get("provider_name") != profile.provider_name or metadata.get("model") != profile.backend_model:
        raise ValueError(f"Wrong returned provider or model for {identifier}")
    saved.write_bytes(body)
    return metadata


def _prime_error(response: dict[str, object], metadata: dict[str, object], profile: WireProfile) -> str | None:
    """Prime serves one route per model: the returned model and Prime's own
    inference ID are the provider evidence."""
    if response.get("model") != profile.backend_model:
        return "Missing or wrong returned model"
    identifier = metadata.get("upstream_request_id")
    if not isinstance(identifier, str) or not identifier:
        return "Missing Prime inference ID"
    return None


def _route_error(response: dict[str, object], routing: dict[str, object] | None, profile: WireProfile) -> str | None:
    if routing is None:
        if response.get("model") != profile.model or response.get("provider") != profile.provider_name:
            return "Missing or wrong returned model or provider"
        return None
    if routing.get("requested") != profile.model:
        return "Wrong requested model in routing metadata"
    endpoints = routing.get("endpoints")
    if not isinstance(endpoints, dict):
        return "Missing routing endpoints"
    available = endpoints.get("available")
    if not isinstance(available, list):
        return "Missing routing endpoints"
    selected = False
    for endpoint in available:
        if not isinstance(endpoint, dict):
            return "Invalid routing endpoint"
        if endpoint.get("selected") is True:
            if endpoint.get("model") != profile.backend_model or endpoint.get("provider") != profile.provider_name:
                return "Wrong selected backend"
            selected = True
    if not selected:
        return "No selected backend"
    attempts = routing.get("attempts")
    if attempts is not None:
        if not isinstance(attempts, list):
            return "Invalid routing attempts"
        for attempt in attempts:
            if not isinstance(attempt, dict):
                return "Invalid routing attempt"
            if attempt.get("model") != profile.backend_model or attempt.get("provider") != profile.provider_name:
                return "Wrong attempted backend"
    return None


def populate_wire_context(logs_dir: Path, context: AgentContext) -> None:
    # Harbor must retain the first parse error, not repeat the hook during recovery.
    context.metadata = {"usage_source": "raw wire responses", "all_recorded_usage_complete": False}
    wire = logs_dir / "wire"
    profile = recorded_profile(wire)
    source = "raw Prime Intellect wire responses" if profile.upstream == "prime" else "raw OpenRouter wire responses"
    run_path = wire / "run.json"
    run = _read_object(run_path) if run_path.exists() else None
    totals = {"input": 0, "cache": 0, "output": 0}
    verified = 0
    complete = 0
    failed = 0
    warmups = 0
    inference = 0
    pending: list[dict[str, str]] = []
    records = sorted(path for path in wire.glob("[0-9]*") if path.is_dir())
    for directory in records:
        metadata_path = directory / "metadata.json"
        if not metadata_path.exists():
            failed += 1
            continue
        metadata = _read_object(metadata_path)
        endpoint = metadata.get("path")
        if not isinstance(endpoint, str):
            raise ValueError(f"Missing endpoint in {directory}")
        if metadata.get("method") == "HEAD":
            warmups += 1
            continue
        if metadata.get("method") != "POST":
            raise ValueError(f"Unsupported recorded request method in {directory}")
        inference += 1
        if metadata.get("status_code") != 200:
            failed += 1
            continue
        _audit_request(directory, endpoint, profile)
        if metadata.get("content_encoding") not in (None, "", "identity"):
            raise ValueError(f"Unexpected response encoding in {directory}")
        response_path = directory / "0001.response.sse"
        content_type = metadata.get("content_type")
        if not isinstance(content_type, str):
            raise ValueError(f"Missing response content type in {directory}")
        events = _events(response_path) if content_type.startswith("text/event-stream") else iter((_read_object(response_path),))
        response: dict[str, object] | None = None
        counters: dict[str, int] = {}
        stopped = False
        routing: dict[str, object] | None = None
        for event in events:
            kind = event.get("type")
            if endpoint == "/api/v1/messages":
                if kind in ("message_start", "message"):
                    response = _object(event.get("message")) if kind == "message_start" else event
                    routing_value = response.get("openrouter_metadata")
                    routing = _object(routing_value) if routing_value is not None else None
                    usage = _object(response.get("usage"))
                    stopped = kind == "message"
                elif kind == "message_delta":
                    usage = _object(event.get("usage"))
                elif kind == "message_stop":
                    stopped = True
                    routing_value = event.get("openrouter_metadata")
                    if routing_value is not None:
                        routing = _object(routing_value)
                    continue
                else:
                    continue
                for field in _TOKEN_FIELDS:
                    value = usage.get(field)
                    if value is not None:
                        count = _count(value)
                        if count > 0 or field not in counters:
                            counters[field] = count
            elif endpoint == "/api/v1/responses":
                if kind in ("response.completed", "response.incomplete", "response.failed"):
                    response = _object(event.get("response"))
                    stopped = True
                elif event.get("object") == "response":
                    response = event
                    stopped = True
            else:
                raise ValueError(f"Unsupported endpoint {endpoint!r}")
        if endpoint == "/api/v1/messages":
            if response is None or not stopped:
                failed += 1
                continue
            route_error = _prime_error(response, metadata, profile) if profile.upstream == "prime" else _route_error(response, routing, profile)
            if route_error is not None:
                raise ValueError(f"{route_error} in {directory}")
            verified += 1
            if "input_tokens" in counters and "output_tokens" in counters:
                complete += 1
                if profile.upstream == "prime":
                    # Prime's input_tokens already includes cache reads (probed:
                    # 7738 input with 7552 cached on a repeated prefix).
                    totals["input"] += counters["input_tokens"]
                else:
                    totals["input"] += sum(counters.get(field, 0) for field in _TOKEN_FIELDS[:3])
                totals["cache"] += counters.get("cache_read_input_tokens", 0)
                totals["output"] += counters["output_tokens"]
            else:
                failed += 1
        elif endpoint == "/api/v1/responses":
            if response is None or not stopped:
                failed += 1
                continue
            if response.get("model") != profile.model or not isinstance(response.get("id"), str):
                raise ValueError(f"Missing returned Responses identity in {directory}")
            identifier = response["id"]
            if not isinstance(identifier, str):
                raise ValueError(f"Invalid response ID in {directory}")
            routing_value = response.get("openrouter_metadata")
            if profile.upstream == "prime":
                route_error = _prime_error(response, metadata, profile)
                if route_error is not None:
                    raise ValueError(f"{route_error} in {directory}")
                verified += 1
            elif routing_value is None:
                pending.append({"record": directory.name, "id": identifier})
            else:
                route_error = _route_error(response, _object(routing_value), profile)
                if route_error is not None:
                    raise ValueError(f"{route_error} in {directory}")
                verified += 1
            usage = _object(response.get("usage"))
            totals["input"] += _count(usage.get("input_tokens"))
            totals["output"] += _count(usage.get("output_tokens"))
            details = _object(usage.get("input_tokens_details"))
            totals["cache"] += _count(details.get("cached_tokens"))
            complete += 1
        else:
            raise ValueError(f"Unsupported endpoint {endpoint!r}")
    all_complete = run is not None and run.get("requests") == len(records) and inference > 0 and complete == inference and failed == 0
    if all_complete:
        context.n_input_tokens = totals["input"]
        context.n_cache_tokens = totals["cache"]
        context.n_output_tokens = totals["output"]
    context.metadata = {
        "usage_source": source,
        "profile": asdict(profile),
        "response_records": len(records),
        "complete_response_records": complete,
        "failed_or_incomplete_records": failed,
        "provider_verified_records": verified,
        "inference_records": inference,
        "warmup_records": warmups,
        "pending_provider_verification": pending,
        "all_recorded_usage_complete": all_complete,
        "observed_complete_usage": totals,
        "process": run,
    }
    wire.mkdir(parents=True, exist_ok=True)
    (wire / "usage.json").write_text(json.dumps(context.metadata, indent=2) + "\n")


def audit_wire_providers(logs_dir: Path) -> None:
    """Verify delayed provider metadata after the native task and grader finish."""
    wire = logs_dir / "wire"
    profile = recorded_profile(wire)
    usage = _read_object(wire / "usage.json")
    if usage.get("all_recorded_usage_complete") is not True:
        raise ValueError(f"Native wire usage is incomplete in {wire}")
    pending = usage.get("pending_provider_verification")
    if not isinstance(pending, list):
        raise ValueError(f"Missing provider audit list in {wire}")
    verified = _count(usage.get("provider_verified_records"))
    for item in pending:
        entry = _object(item)
        identifier, name = entry.get("id"), entry.get("record")
        if not isinstance(identifier, str) or not isinstance(name, str) or not name.isdecimal():
            raise ValueError(f"Invalid provider audit entry in {wire}")
        _generation_metadata(identifier, wire / name, profile)
        verified += 1
    if verified != usage.get("inference_records"):
        raise ValueError(f"Missing provider evidence in {wire}")
    (wire / "provider-audit.json").write_text(json.dumps({"verified_inference_records": verified, "provider": profile.provider_name, "model": profile.model, "backend_model": profile.backend_model}, indent=2) + "\n")


def main() -> int:
    parser = argparse.ArgumentParser(description="Verify native provider records after a Harbor job finishes.")
    parser.add_argument("job_dir", type=Path)
    arguments = parser.parse_args()
    trials = sorted(path for path in arguments.job_dir.iterdir() if path.is_dir() and (path / "config.json").is_file())
    if not trials:
        parser.error("No native trials were found")
    failures: list[dict[str, str]] = []
    for trial in trials:
        try:
            audit_wire_providers(trial / "agent")
        except (OSError, ValueError) as error:
            failures.append({"trial": trial.name, "error": str(error)})
    print(json.dumps({"trials": len(trials), "verified_trials": len(trials) - len(failures), "failed_trials": failures}, indent=2))
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
