"""Checks that recorded wire traffic becomes the right training conversations."""

import io
import json
from contextlib import redirect_stdout
from pathlib import Path
import tempfile
import unittest

from evals import tinker_dataset

TOOLS = [{"name": "bash", "description": "Run a command.", "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}}}]


def _sse(events: list[dict[str, object]]) -> str:
    return "".join(f"event: {event['type']}\ndata: {json.dumps(event)}\n\n" for event in events)


def _reply(thinking: str, command: str | None = None, text: str | None = None, call_id: str = "call_1") -> str:
    events: list[dict[str, object]] = [{"type": "message_start", "message": {"usage": {}}}]
    events += [
        {"type": "content_block_start", "index": 0, "content_block": {"type": "thinking", "thinking": "", "signature": ""}},
        {"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": thinking[:3]}},
        {"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": thinking[3:]}},
    ]
    if command is not None:
        arguments = json.dumps({"command": command})
        events += [
            {"type": "content_block_start", "index": 1, "content_block": {"type": "tool_use", "id": call_id, "name": "bash", "input": {}}},
            {"type": "content_block_delta", "index": 1, "delta": {"type": "input_json_delta", "partial_json": arguments[:5]}},
            {"type": "content_block_delta", "index": 1, "delta": {"type": "input_json_delta", "partial_json": arguments[5:]}},
        ]
    if text is not None:
        events += [
            {"type": "content_block_start", "index": 2, "content_block": {"type": "text", "text": ""}},
            {"type": "content_block_delta", "index": 2, "delta": {"type": "text_delta", "text": text}},
        ]
    return _sse(events + [{"type": "message_stop"}])


def _record(wire: Path, number: int, endpoint: str, request: dict[str, object], response: str, method: str = "POST") -> None:
    directory = wire / f"{number:04d}"
    directory.mkdir(parents=True)
    (directory / "metadata.json").write_text(json.dumps({"path": endpoint, "method": method, "status_code": 200}))
    (directory / "0001.request.json").write_text(json.dumps(request))
    (directory / "0001.response.sse").write_text(response)


def _trial(job: Path, name: str, task: str, reward: float, harness: str = "inkling") -> Path:
    trial = job / name
    (trial / "agent" / "wire").mkdir(parents=True)
    result = {
        "task_name": task,
        "verifier_result": {"rewards": {"reward": reward}},
        "agent_info": {"name": harness, "version": "1", "model_info": {"provider": "thinkingmachines", "name": "inkling-small"}},
    }
    (trial / "result.json").write_text(json.dumps(result))
    return trial / "agent" / "wire"


class MessagesThreads(unittest.TestCase):
    def test_calls_extend_one_thread_and_keep_reply_thinking(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            wire = Path(root)
            task = {"role": "user", "content": [{"type": "text", "text": "Fix it."}]}
            _record(wire, 1, "/api/v1/messages", {}, "", method="HEAD")
            _record(wire, 2, "/api/v1/messages", {"system": "Be brief.", "tools": TOOLS, "messages": [task]}, _reply("look first", "ls"))
            # The harness drops thinking from history; the thread keeps it from the reply.
            second = [
                task,
                {"role": "assistant", "content": [{"type": "tool_use", "id": "call_1", "name": "bash", "input": {"command": "ls"}}]},
                {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "call_1", "content": [{"type": "text", "text": "a.py"}]}]},
            ]
            _record(wire, 3, "/api/v1/messages", {"system": "Be brief.", "tools": TOOLS, "messages": second}, _reply("all done", text="Fixed."))
            threads = tinker_dataset.threads(wire)
        self.assertEqual(len(threads), 1)
        messages = threads[0]["messages"]
        self.assertEqual([message["role"] for message in messages], ["system", "user", "assistant", "tool", "assistant"])
        self.assertEqual(messages[2]["content"], [{"type": "thinking", "thinking": "look first"}])
        self.assertEqual(messages[2]["tool_calls"][0]["function"], {"name": "bash", "arguments": json.dumps({"command": "ls"})})
        self.assertEqual(messages[3], {"role": "tool", "tool_call_id": "call_1", "content": "a.py"})
        self.assertEqual(messages[4]["content"], [{"type": "thinking", "thinking": "all done"}, {"type": "text", "text": "Fixed."}])
        self.assertEqual(threads[0]["tools"][0]["parameters"], TOOLS[0]["input_schema"])
        self.assertEqual(threads[0]["calls"], 2)

    def test_compacted_history_starts_a_new_thread(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            wire = Path(root)
            _record(wire, 1, "/api/v1/messages", {"messages": [{"role": "user", "content": "Fix it."}]}, _reply("first", "ls"))
            _record(wire, 2, "/api/v1/messages", {"messages": [{"role": "user", "content": "Summary so far: listed files."}]}, _reply("next", "cat a.py"))
            threads = tinker_dataset.threads(wire)
        self.assertEqual(len(threads), 2)
        self.assertEqual(threads[1]["messages"][0]["content"], "Summary so far: listed files.")

    def test_unfinished_stream_is_skipped(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            wire = Path(root)
            truncated = _reply("cut", "ls").replace('event: message_stop\ndata: {"type": "message_stop"}\n\n', "")
            _record(wire, 1, "/api/v1/messages", {"messages": [{"role": "user", "content": "Go."}]}, truncated)
            self.assertEqual(tinker_dataset.threads(wire), [])


class ResponsesThreads(unittest.TestCase):
    def test_codex_reasoning_calls_and_outputs(self) -> None:
        request = {
            "instructions": "You are Codex.",
            "tools": [{"type": "function", "name": "exec_command", "parameters": {"type": "object"}}, {"type": "web_search"}],
            "input": [
                {"type": "message", "role": "developer", "content": [{"type": "input_text", "text": "Sandbox rules."}]},
                {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "Fix it."}]},
                {"type": "reasoning", "summary": [], "content": [{"type": "reasoning_text", "text": "inspect"}]},
                {"type": "function_call", "name": "exec_command", "arguments": "{\"cmd\": \"ls\"}", "call_id": "c1"},
                {"type": "function_call_output", "call_id": "c1", "output": "a.py"},
            ],
        }
        completed = {"type": "response.completed", "response": {"output": [
            {"type": "reasoning", "summary": [{"type": "summary_text", "text": "edit"}]},
            {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Done."}]},
        ]}}
        with tempfile.TemporaryDirectory() as root:
            wire = Path(root)
            _record(wire, 1, "/api/v1/responses", request, _sse([{"type": "response.created"}, completed]))
            (thread,) = tinker_dataset.threads(wire)
        roles = [message["role"] for message in thread["messages"]]
        self.assertEqual(roles, ["system", "system", "user", "assistant", "tool", "assistant"])
        self.assertEqual(thread["messages"][3]["content"], [{"type": "thinking", "thinking": "inspect"}])
        self.assertEqual(thread["messages"][3]["tool_calls"][0]["id"], "c1")
        self.assertEqual(thread["messages"][5]["content"], [{"type": "thinking", "thinking": "edit"}, {"type": "text", "text": "Done."}])
        self.assertEqual([tool["name"] for tool in thread["tools"]], ["exec_command"])


class Export(unittest.TestCase):
    def test_passing_trials_split_by_task(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            job = Path(root) / "job"
            tasks = [f"task-{index}" for index in range(40)]
            for index, task in enumerate(tasks):
                for attempt, reward in enumerate((1.0, 0.0)):
                    wire = _trial(job, f"{task}__{attempt}", task, reward)
                    first = [{"role": "user", "content": f"Solve {task}."}]
                    _record(wire, 1, "/api/v1/messages", {"messages": first}, _reply("plan", "ls"))
                    second = first + [
                        {"role": "assistant", "content": [{"type": "tool_use", "id": "call_1", "name": "bash", "input": {"command": "ls"}}]},
                        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "call_1", "content": "ok"}]},
                    ]
                    _record(wire, 2, "/api/v1/messages", {"messages": second}, _reply("done", text="Solved."))
            train, held = Path(root) / "train.jsonl", Path(root) / "eval.jsonl"
            with redirect_stdout(io.StringIO()) as output:
                code = tinker_dataset.main([str(job), "--out", str(train), "--eval-out", str(held)])
            summary = json.loads(output.getvalue())
            train_rows = [json.loads(line) for line in train.read_text().splitlines()]
            held_rows = [json.loads(line) for line in held.read_text().splitlines()]
        self.assertEqual(code, 0)
        self.assertEqual(summary["rows"]["train"] + summary["rows"]["eval"], 40)
        self.assertTrue(all(row["meta"]["reward"] == 1.0 for row in train_rows + held_rows))
        self.assertFalse({row["meta"]["task"] for row in train_rows} & {row["meta"]["task"] for row in held_rows})
        self.assertTrue(held_rows, "the default 20% task split should hold some tasks out")
        self.assertEqual(train_rows[0]["meta"]["harness"], "inkling")
        self.assertEqual(train_rows[0]["meta"]["model"], "thinkingmachines/inkling-small")


if __name__ == "__main__":
    unittest.main()
