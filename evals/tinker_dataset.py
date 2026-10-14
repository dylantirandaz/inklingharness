"""Turn recorded Harbor trials into Tinker training conversations.

Every native trial keeps its model traffic under agent/wire: one numbered
record per call, holding the request the harness sent and the streamed reply.
A trial's calls are rebuilt into conversation threads: a call whose request
extends a thread's history continues that thread, and any other call (after
compaction, or a side request such as a title) starts a new one. Each thread
becomes one JSONL row in the tinker-cookbook chat format, tagged with the
trial's task, harness, model and verifier reward.

Only the standard library is used, so this runs in any environment.
"""

import argparse
from collections.abc import Iterable, Iterator
import hashlib
import json
from pathlib import Path
import sys

MESSAGES = "/api/v1/messages"
RESPONSES = "/api/v1/responses"


def _text(value: object) -> str:
    """Flatten Anthropic or Responses content into plain text; images are dropped."""
    if value is None:
        return ""
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        parts = []
        for block in value:
            if isinstance(block, str):
                parts.append(block)
            elif isinstance(block, dict) and isinstance(block.get("text"), str):
                parts.append(block["text"])
        return "\n".join(parts)
    raise ValueError(f"Unsupported content {type(value).__name__}")


def _arguments(value: object) -> str:
    return value if isinstance(value, str) else json.dumps(value if value is not None else {}, ensure_ascii=False)


def _assistant(thinking: list[str], text: list[str], calls: list[dict[str, object]]) -> dict[str, object]:
    content: list[dict[str, str]] = [{"type": "thinking", "thinking": item} for item in thinking if item]
    content += [{"type": "text", "text": item} for item in text if item]
    message: dict[str, object] = {"role": "assistant", "content": content}
    if calls:
        message["tool_calls"] = calls
    return message


def _call(identifier: object, name: object, arguments: object) -> dict[str, object]:
    if not isinstance(name, str):
        raise ValueError("Tool call without a name")
    return {"type": "function", "id": identifier if isinstance(identifier, str) else None, "function": {"name": name, "arguments": _arguments(arguments)}}


# Anthropic Messages


def _messages_history(request: dict[str, object]) -> list[dict[str, object]]:
    history: list[dict[str, object]] = []
    system = _text(request.get("system"))
    if system:
        history.append({"role": "system", "content": system})
    for message in request.get("messages") or []:
        role, content = message.get("role"), message.get("content")
        blocks = [{"type": "text", "text": content}] if isinstance(content, str) else content or []
        if role == "assistant":
            thinking = [b.get("thinking", "") for b in blocks if b.get("type") == "thinking"]
            text = [b.get("text", "") for b in blocks if b.get("type") == "text"]
            calls = [_call(b.get("id"), b.get("name"), b.get("input")) for b in blocks if b.get("type") == "tool_use"]
            history.append(_assistant(thinking, text, calls))
        elif role == "user":
            text = []
            for block in blocks:
                if block.get("type") == "tool_result":
                    history.append({"role": "tool", "tool_call_id": block.get("tool_use_id"), "content": _text(block.get("content"))})
                elif block.get("type") == "text":
                    text.append(block.get("text", ""))
            if text:
                history.append({"role": "user", "content": "\n".join(text)})
        elif role == "system":
            history.append({"role": "system", "content": _text(blocks)})
        else:
            raise ValueError(f"Unsupported message role {role!r}")
    return history


def _messages_tools(request: dict[str, object]) -> list[dict[str, object]]:
    tools = []
    for tool in request.get("tools") or []:
        if "input_schema" in tool:
            tools.append({"name": tool["name"], "description": tool.get("description", ""), "parameters": tool["input_schema"]})
    return tools


def _messages_reply(events: Iterable[dict[str, object]]) -> dict[str, object] | None:
    blocks: dict[int, dict[str, object]] = {}
    stopped = False
    for event in events:
        kind = event.get("type")
        if kind == "message":  # non-streamed reply
            return _messages_history({"messages": [{"role": "assistant", "content": event.get("content") or []}]})[0]
        if kind == "content_block_start":
            block = dict(event["content_block"])
            if block.get("type") == "tool_use":
                block["partial"] = ""
            blocks[event["index"]] = block
        elif kind == "content_block_delta":
            block, delta = blocks[event["index"]], event["delta"]
            if delta.get("type") == "thinking_delta":
                block["thinking"] = block.get("thinking", "") + delta.get("thinking", "")
            elif delta.get("type") == "text_delta":
                block["text"] = block.get("text", "") + delta.get("text", "")
            elif delta.get("type") == "input_json_delta":
                block["partial"] = block.get("partial", "") + delta.get("partial_json", "")
        elif kind == "message_stop":
            stopped = True
    if not stopped:
        return None
    content = []
    for index in sorted(blocks):
        block = blocks[index]
        if block.get("type") == "tool_use" and block.get("partial"):
            block["input"] = json.loads(block["partial"])
        content.append(block)
    return _messages_history({"messages": [{"role": "assistant", "content": content}]})[0]


# OpenAI Responses


def _responses_items(items: list[dict[str, object]]) -> list[dict[str, object]]:
    history: list[dict[str, object]] = []
    thinking: list[str] = []
    text: list[str] = []
    calls: list[dict[str, object]] = []

    def flush() -> None:
        nonlocal thinking, text, calls
        if thinking or text or calls:
            history.append(_assistant(thinking, text, calls))
        thinking, text, calls = [], [], []

    for item in items:
        kind = item.get("type") or "message"
        if kind == "reasoning":
            if text or calls:
                flush()
            thinking.append(_text(item.get("content")) or _text(item.get("summary")))
        elif kind == "function_call":
            calls.append(_call(item.get("call_id"), item.get("name"), item.get("arguments")))
        elif kind == "message" and item.get("role") == "assistant":
            if calls:
                flush()
            text.append(_text(item.get("content")))
        elif kind in ("function_call_output", "custom_tool_call_output"):
            flush()
            history.append({"role": "tool", "tool_call_id": item.get("call_id"), "content": _text(item.get("output"))})
        elif kind == "message":
            flush()
            role = item.get("role")
            history.append({"role": "system" if role == "developer" else role, "content": _text(item.get("content"))})
        else:
            flush()  # web search calls and other hosted items have no chat form
    flush()
    return history


def _responses_history(request: dict[str, object]) -> list[dict[str, object]]:
    history: list[dict[str, object]] = []
    if request.get("instructions"):
        history.append({"role": "system", "content": _text(request["instructions"])})
    items = request.get("input")
    if isinstance(items, str):
        items = [{"type": "message", "role": "user", "content": items}]
    return history + _responses_items(items or [])


def _responses_tools(request: dict[str, object]) -> list[dict[str, object]]:
    tools = []
    pending = list(request.get("tools") or [])
    while pending:
        tool = pending.pop(0)
        if tool.get("type") == "function":
            tools.append({"name": tool["name"], "description": tool.get("description", ""), "parameters": tool.get("parameters") or {}})
        elif tool.get("type") == "namespace":
            pending[:0] = list(tool.get("tools") or [])
    return tools


def _responses_reply(events: Iterable[dict[str, object]]) -> dict[str, object] | None:
    for event in events:
        response = event.get("response") if event.get("type") in ("response.completed", "response.incomplete") else event if event.get("object") == "response" else None
        if isinstance(response, dict):
            replies = _responses_items(response.get("output") or [])
            return replies[0] if len(replies) == 1 and replies[0]["role"] == "assistant" else None
    return None


# Records and threads


def _events(path: Path) -> Iterator[dict[str, object]]:
    text = path.read_text()
    if not text.strip():
        return
    if not text.lstrip().startswith(("event:", "data:", ":")):
        yield json.loads(text)
        return
    for line in text.splitlines():
        if line.startswith("data:"):
            payload = line[5:].strip()
            if payload and payload != "[DONE]":
                yield json.loads(payload)


def calls(wire: Path) -> Iterator[tuple[list[dict[str, object]], list[dict[str, object]], dict[str, object]]]:
    """Yield (history, tools, reply) for each completed inference call, in order."""
    for record in sorted(path for path in wire.glob("[0-9]*") if path.is_dir()):
        metadata_path = record / "metadata.json"
        if not metadata_path.exists():
            continue
        metadata = json.loads(metadata_path.read_text())
        if metadata.get("method") != "POST" or metadata.get("status_code") != 200:
            continue
        endpoint = metadata.get("path")
        request = json.loads((record / "0001.request.json").read_text())
        events = _events(record / "0001.response.sse")
        if endpoint == MESSAGES:
            history, tools, reply = _messages_history(request), _messages_tools(request), _messages_reply(events)
        elif endpoint == RESPONSES:
            history, tools, reply = _responses_history(request), _responses_tools(request), _responses_reply(events)
        else:
            continue
        if reply is not None:
            yield history, tools, reply


def _key(message: dict[str, object]) -> str:
    """Identify a message regardless of thinking, which harnesses may drop from history."""
    content = message.get("content")
    if isinstance(content, list):
        content = "\n".join(part.get("text", "") for part in content if part.get("type") == "text")
    calls = [(call["function"]["name"], json.loads(call["function"]["arguments"] or "{}")) for call in message.get("tool_calls") or []]
    return json.dumps([message["role"], (content or "").strip(), calls], sort_keys=True, ensure_ascii=False)


def threads(wire: Path) -> list[dict[str, object]]:
    """Rebuild conversation threads; recorded replies keep their thinking."""
    open_threads: list[dict[str, object]] = []
    for history, tools, reply in calls(wire):
        keys = [_key(message) for message in history]
        best = None
        for thread in open_threads:
            size = len(thread["keys"])
            if size <= len(keys) and keys[:size] == thread["keys"] and (best is None or size > len(best["keys"])):
                best = thread
        if best is None:
            best = {"messages": [], "keys": [], "tools": tools, "calls": 0}
            open_threads.append(best)
        size = len(best["keys"])
        best["messages"] = best["messages"] + history[size:] + [reply]
        best["keys"] = keys + [_key(reply)]
        best["tools"] = tools
        best["calls"] += 1
    return open_threads


def _reward(trial: Path) -> float | None:
    result = json.loads((trial / "result.json").read_text())
    rewards = (result.get("verifier_result") or {}).get("rewards") or {}
    reward = rewards.get("reward")
    return float(reward) if isinstance(reward, (int, float)) else None


def _identity(trial: Path) -> dict[str, str]:
    result = json.loads((trial / "result.json").read_text())
    agent = result.get("agent_info") or {}
    model = (agent.get("model_info") or {})
    def config() -> dict[str, object]:
        return result.get("config") or json.loads((trial / "config.json").read_text())

    return {
        "task": result.get("task_name") or config()["task"]["path"],
        "harness": agent.get("name") or config()["agent"].get("import_path", ""),
        "harness_version": agent.get("version") or "",
        "model": "/".join(part for part in (model.get("provider"), model.get("name")) if part) or config()["agent"].get("model_name", ""),
    }


def holdout(task: str, fraction: float) -> bool:
    """Deterministic task-level split, so a task's runs never straddle train and eval."""
    digest = hashlib.sha256(task.encode()).digest()
    return int.from_bytes(digest[:8], "big") / 2**64 < fraction


def rows(job_dirs: Iterable[Path], min_reward: float | None, min_calls: int, models: set[str], errors: list[dict[str, str]]) -> Iterator[dict[str, object]]:
    for job in job_dirs:
        for trial in sorted(path for path in job.iterdir() if (path / "result.json").is_file()):
            wire = trial / "agent" / "wire"
            if not wire.is_dir():
                continue
            reward = _reward(trial)
            if reward is None or (min_reward is not None and reward < min_reward):
                continue
            identity = _identity(trial)
            if models and identity["model"] not in models:
                continue
            try:
                found = [thread for thread in threads(wire) if thread["calls"] >= min_calls]
            except (OSError, ValueError, KeyError, TypeError, AttributeError) as error:
                errors.append({"trial": f"{job.name}/{trial.name}", "error": f"{type(error).__name__}: {error}"})
                continue
            for index, thread in enumerate(found):
                yield {
                    "messages": thread["messages"],
                    "tools": thread["tools"],
                    "meta": {**identity, "reward": reward, "job": job.name, "trial": trial.name, "thread": index, "threads": len(found), "calls": thread["calls"]},
                }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("job_dirs", nargs="+", type=Path, help="Harbor job directories")
    parser.add_argument("--out", type=Path, required=True, help="Training JSONL to write")
    parser.add_argument("--eval-out", type=Path, help="Held-out JSONL for tasks in the eval split")
    parser.add_argument("--eval-fraction", type=float, default=0.2, help="Share of tasks held out (by task name hash)")
    parser.add_argument("--min-reward", type=float, default=1.0, help="Keep trials scoring at least this; pass -1 for all")
    parser.add_argument("--min-calls", type=int, default=2, help="Drop threads with fewer model calls (titles, probes)")
    parser.add_argument("--model", action="append", default=[], help="Keep only this recorded model (repeatable)")
    arguments = parser.parse_args(argv)
    minimum = None if arguments.min_reward < 0 else arguments.min_reward
    counts = {"train": 0, "eval": 0}
    errors: list[dict[str, str]] = []
    tasks: dict[str, set[str]] = {"train": set(), "eval": set()}
    arguments.out.parent.mkdir(parents=True, exist_ok=True)
    eval_file = arguments.eval_out.open("w") if arguments.eval_out else None
    with arguments.out.open("w") as train_file:
        for row in rows(arguments.job_dirs, minimum, arguments.min_calls, set(arguments.model), errors):
            split = "eval" if holdout(row["meta"]["task"], arguments.eval_fraction) else "train"
            target = train_file if split == "train" else eval_file
            if target is None:
                continue
            target.write(json.dumps(row, ensure_ascii=False) + "\n")
            counts[split] += 1
            tasks[split].add(row["meta"]["task"])
    if eval_file:
        eval_file.close()
    json.dump({"rows": counts, "tasks": {key: len(value) for key, value in tasks.items()}, "skipped_trials": errors}, sys.stdout, indent=2)
    print()
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(main())
