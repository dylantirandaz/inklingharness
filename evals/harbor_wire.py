"""Read complete native wire evidence; missing usage is not zero usage."""

import argparse
import asyncio
from collections.abc import Iterator
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
    try:
        await agent.exec_as_agent(environment, command=command, env=env)
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


def _audit_request(directory: Path, endpoint: str) -> None:
    incoming = _read_object(directory / "incoming.json")
    forwarded = _read_object(directory / "0001.request.json")
    if forwarded.get("model") != MODEL or incoming.get("model") != MODEL:
        raise ValueError(f"Wrong model in {directory}")
    if forwarded.get("provider") != {"order": ["deepinfra/fp8"], "allow_fallbacks": False}:
        raise ValueError(f"Wrong provider profile in {directory}")
    expected = dict(incoming)
    expected["provider"] = {"order": ["deepinfra/fp8"], "allow_fallbacks": False}
    if endpoint == "/api/v1/messages":
        if forwarded.get("max_tokens") != 16384 or _object(forwarded.get("output_config")).get("effort") != "high":
            raise ValueError(f"Wrong Messages profile in {directory}")
        expected["max_tokens"] = 16384
        output = dict(_object(incoming["output_config"])) if "output_config" in incoming else {}
        output["effort"] = "high"
        expected["output_config"] = output
        for field in ("thinking", "temperature"):
            if field not in forwarded:
                expected.pop(field, None)
    elif endpoint == "/api/v1/responses":
        if forwarded.get("max_output_tokens") != 16384 or _object(forwarded.get("reasoning")).get("effort") != "high":
            raise ValueError(f"Wrong Responses profile in {directory}")
        expected["max_output_tokens"] = 16384
    else:
        raise ValueError(f"Unsupported recorded endpoint {endpoint!r}")
    if forwarded != expected:
        raise ValueError(f"The recorder changed a prompt, tool, history, or other non-profile field in {directory}")


def _generation_metadata(identifier: str, directory: Path) -> dict[str, object]:
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
    if metadata.get("id") != identifier or metadata.get("provider_name") != "DeepInfra" or metadata.get("model") != "thinkingmachines/inkling-small-20260730":
        raise ValueError(f"Wrong returned provider or model for {identifier}")
    saved.write_bytes(body)
    return metadata


def populate_wire_context(logs_dir: Path, context: AgentContext) -> None:
    wire = logs_dir / "wire"
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
        _audit_request(directory, endpoint)
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
        provider_verified = False
        for event in events:
            kind = event.get("type")
            if endpoint == "/api/v1/messages":
                if kind in ("message_start", "message"):
                    message = _object(event.get("message")) if kind == "message_start" else event
                    if message.get("model") != MODEL or message.get("provider") != "DeepInfra":
                        raise ValueError(f"Wrong returned Messages model or provider in {directory}")
                    provider_verified = True
                    usage = _object(message.get("usage"))
                    stopped = kind == "message"
                elif kind == "message_delta":
                    usage = _object(event.get("usage"))
                elif kind == "message_stop":
                    stopped = True
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
            verified += provider_verified
            if stopped and provider_verified and "input_tokens" in counters and "output_tokens" in counters:
                complete += 1
                totals["input"] += sum(counters.get(field, 0) for field in _TOKEN_FIELDS[:3])
                totals["cache"] += counters.get("cache_read_input_tokens", 0)
                totals["output"] += counters["output_tokens"]
            else:
                failed += 1
        elif endpoint == "/api/v1/responses":
            if response is None or not stopped:
                failed += 1
                continue
            if response.get("model") != MODEL or not isinstance(response.get("id"), str):
                raise ValueError(f"Missing returned Responses identity in {directory}")
            identifier = response["id"]
            if not isinstance(identifier, str):
                raise ValueError(f"Invalid response ID in {directory}")
            pending.append({"record": directory.name, "id": identifier})
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
        "usage_source": "raw OpenRouter wire responses",
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
    (wire / "usage.json").write_text(json.dumps(context.metadata, indent=2) + "\n")


def audit_wire_providers(logs_dir: Path) -> None:
    """Verify delayed provider metadata after the native task and grader finish."""
    wire = logs_dir / "wire"
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
        _generation_metadata(identifier, wire / name)
        verified += 1
    if verified != usage.get("inference_records"):
        raise ValueError(f"Missing provider evidence in {wire}")
    (wire / "provider-audit.json").write_text(json.dumps({"verified_inference_records": verified, "provider": "DeepInfra", "model": MODEL}, indent=2) + "\n")


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
