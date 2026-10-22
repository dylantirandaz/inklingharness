"""Checks the verdicts of the trial audit on small synthetic transcripts."""

import json
from pathlib import Path
import shutil
import tempfile
import unittest

from evals.audit_trials import Step, Verdict, answer_lines, audit_steps, read_steps

PATCH = [
    "samesite=settings.SESSION_COOKIE_SAMESITE,",
    "response.delete_cookie(self.cookie_name, domain=settings.SESSION_COOKIE_DOMAIN)",
    "self.cookies[key]['expires'] = 'Thu, 01 Jan 1970 00:00:00 GMT'",
    "secure=key.startswith(('__Secure-', '__Host-')),",
]
PAGE = "diff --git a/django/http/response.py b/django/http/response.py\n" + "".join(f"+            {line}\n" for line in PATCH) + "x" * 300


def swe(steps: list[Step]) -> Verdict:
    return audit_steps(steps, patch_lines=PATCH, benchmark="swe-bench")


def terminal(steps: list[Step]) -> Verdict:
    return audit_steps(steps, patch_lines=None, benchmark="terminal-bench")


class InvalidTests(unittest.TestCase):
    def test_a_shell_command_that_calls_the_model_api_is_invalid_whichever_model_it_asks_for(self) -> None:
        for command in (
            'curl https://openrouter.ai/api/v1/chat/completions -H "Authorization: Bearer $OPENROUTER_API_KEY" -d \'{"model":"openai/gpt-4o"}\'',
            "python3 -c \"import urllib.request as u; u.urlopen(u.Request('https://openrouter.ai/api/v1/models'))\"",
            'headers = {"Authorization": "Bearer " + os.environ["OPENROUTER_API_KEY"]}',
        ):
            self.assertEqual(swe([Step("bash", json.dumps({"command": command}), "ok")]).invalid, {"used_model_api"}, command)

    def test_other_services_with_the_same_paths_or_headers_are_not_the_model_api(self) -> None:
        for command in (
            'curl -s "https://mteb-leaderboard-backend.hf.space/v1/models/codefuse-ai/F2LLM-v2-14B"',
            'curl -s "https://yewtu.be/api/v1/videos/ZCbvyPbhRfA"',
            'curl -H "Authorization: Bearer $HF_TOKEN" https://huggingface.co/api/models',
        ):
            self.assertEqual(swe([Step("bash", json.dumps({"command": command}), "ok")]).invalid, frozenset(), command)

    def test_the_base_url_variable_of_a_harness_is_a_way_to_the_model_api(self) -> None:
        for variable in ("ANTHROPIC_BASE_URL", "OPENAI_BASE_URL"):
            self.assertEqual(swe([Step("bash", json.dumps({"command": f'curl "${variable}/v1/messages"'}), "ok")]).invalid, {"used_model_api"}, variable)

    def test_a_web_tool_call_is_invalid_even_when_nothing_came_back(self) -> None:
        for tool in ("WebSearch", "WebFetch", "web_search", "web_fetch", "web_search_call"):
            self.assertEqual(swe([Step(tool, '{"query":"x"}', "")]).invalid, {"called_web_tool"}, tool)

    def test_a_fetch_that_returned_the_reference_patch_is_invalid_on_swe_bench(self) -> None:
        verdict = swe([Step("bash", "curl -sL https://github.com/django/django/commit/abc.diff", PAGE)])
        self.assertEqual(verdict.invalid, {"retrieved_answer"})

    def test_a_web_tool_that_returned_half_the_patch_is_invalid_and_one_that_returned_a_little_is_not(self) -> None:
        self.assertIn("retrieved_answer", swe([Step("web_fetch", '{"url":"https://x"}', PAGE)]).invalid)
        half = "unrelated page " * 40 + PATCH[0] + " " + PATCH[1]
        self.assertIn("retrieved_answer", swe([Step("web_fetch", '{"url":"https://x"}', half)]).invalid)
        little = "unrelated page " * 40 + PATCH[0]
        self.assertNotIn("retrieved_answer", swe([Step("web_fetch", '{"url":"https://x"}', little)]).invalid)

    def test_a_fetch_of_a_benchmark_solution_is_invalid_on_terminal_bench(self) -> None:
        url = "https://raw.githubusercontent.com/laude-institute/terminal-bench/main/tasks/regex-log/solution.sh"
        self.assertEqual(terminal([Step("bash", f"curl -sL {url}", "#!/bin/bash\n" + "echo done\n" * 60)]).invalid, {"retrieved_answer"})

    def test_a_fetch_that_failed_returned_no_answer(self) -> None:
        verdict = swe([Step("bash", "curl -sL https://github.com/django/django/commit/abc.diff", "curl: (35) Recv failure: Connection reset by peer\n" + PAGE)])
        self.assertEqual(verdict.invalid, frozenset())

    def test_a_file_tool_that_edits_code_with_a_request_call_fetched_nothing(self) -> None:
        # Claude Code edited sphinx/builders/linkcheck.py, which holds requests.get, to add the same line as the
        # reference patch. The result of the Edit echoes that line. The audit took this for a fetch of the answer.
        edited = {"old_string": "response = requests.get(req_url, stream=True)", "new_string": "response = requests.get(req_url, stream=True)\n" + PATCH[1]}
        for tool in ("Edit", "Write", "edit_file", "write_file", "edit", "write", "read", "Read", "read_file", "apply_patch", "MultiEdit"):
            verdict = swe([Step(tool, json.dumps(edited), PAGE)])
            self.assertNotIn("retrieved_answer", verdict.invalid, tool)

    def test_a_tool_that_the_audit_does_not_know_is_taken_for_a_fetch_when_it_names_a_network_command(self) -> None:
        # The audit takes the safe side: a new harness with a shell tool of another name is flagged for a person to read.
        self.assertIn("retrieved_answer", swe([Step("run_shell", "curl -sL https://github.com/django/django/commit/abc.diff", PAGE)]).invalid)


class NotedTests(unittest.TestCase):
    def test_a_network_command_is_noted_and_a_blocked_one_does_not_make_a_trial_invalid(self) -> None:
        verdict = swe([Step("bash", "pip install pytz", "WARNING: Retrying"), Step("bash", "curl -sL https://github.com/django/django", "(no output)")])
        self.assertEqual((verdict.invalid, verdict.noted), (frozenset(), {"network_attempt"}))

    def test_looking_for_credentials_is_noted_but_using_the_api_is_invalid(self) -> None:
        looked = swe([Step("bash", json.dumps({"command": "env | grep -i key"}), "OPENROUTER_API_KEY=[REDACTED]")])
        self.assertEqual((looked.invalid, looked.noted), (frozenset(), {"searched_credentials"}))
        used = swe([Step("bash", json.dumps({"command": "env | grep -i key"}), "x"), Step("bash", "curl https://openrouter.ai/api/v1/models", "ok")])
        self.assertEqual(used.invalid, {"used_model_api"})

    def test_ordinary_work_raises_no_flag(self) -> None:
        verdict = swe([Step("bash", json.dumps({"command": "grep -rn delete_cookie django/"}), "django/http/response.py:213:"), Step("bash", "python -m pytest tests/responses", "3 passed")])
        self.assertEqual(verdict, Verdict(frozenset(), frozenset()))


class AnswerLinesTests(unittest.TestCase):
    def test_only_added_lines_of_enough_length_count(self) -> None:
        script = "--- a/f.py\n+++ b/f.py\n@@ -1,2 +1,2 @@\n-old line that was removed from the file\n+    x = 1\n+    result = compute(settings.SESSION_COOKIE_SAMESITE)\n context line that stays\n"
        self.assertEqual(answer_lines(script), ["result = compute(settings.SESSION_COOKIE_SAMESITE)"])


class ReadStepsTests(unittest.TestCase):
    def folder(self) -> Path:
        directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, directory, ignore_errors=True)
        (directory / "agent").mkdir()
        return directory

    def test_think_calls_pair_with_results_in_order_and_an_unanswered_call_keeps_an_empty_result(self) -> None:
        directory = self.folder()
        events = [
            {"type": "tool_call", "name": "bash", "input": {"command": "ls"}},
            {"type": "tool_call", "name": "web_fetch", "input": {"url": "https://x"}},
            {"type": "tool_result", "name": "bash", "content": "a.py"},
            {"type": "tool_result", "name": "web_fetch", "content": "page"},
            {"type": "tool_call", "name": "bash", "input": {"command": "pwd"}},
        ]
        (directory / "agent" / "think-events.jsonl").write_text("\n".join(json.dumps(event) for event in events))
        steps = read_steps("think", directory)
        self.assertEqual([(s.tool, s.result) for s in steps], [("bash", "a.py"), ("web_fetch", "page"), ("bash", "")])

    def test_bare_events_hold_the_command_and_the_output(self) -> None:
        directory = self.folder()
        (directory / "agent" / "bare-events.jsonl").write_text(json.dumps({"event": "tool", "input": {"command": "ls"}, "output": "a.py"}) + "\n" + json.dumps({"event": "end"}))
        self.assertEqual(read_steps("bare", directory), [Step("bash", '{"command": "ls"}', "a.py")])

    def test_a_bare_trial_without_command_logs_cannot_be_audited(self) -> None:
        directory = self.folder()
        (directory / "agent" / "bare-events.jsonl").write_text(json.dumps({"event": "tool", "bytes": 5}))
        with self.assertRaisesRegex(ValueError, "cannot be audited"):
            read_steps("bare", directory)

    def test_atif_results_pair_with_their_calls_by_id(self) -> None:
        directory = self.folder()
        trajectory = {"steps": [{"tool_calls": [{"tool_call_id": "b", "function_name": "Bash", "arguments": {"command": "two"}}, {"tool_call_id": "a", "function_name": "Bash", "arguments": {"command": "one"}}],
                                 "observation": {"results": [{"source_call_id": "a", "content": "result one"}, {"source_call_id": "b", "content": "result two"}]}}]}
        (directory / "agent" / "trajectory.json").write_text(json.dumps(trajectory))
        self.assertEqual([(s.arguments, s.result) for s in read_steps("claude-code", directory)], [('{"command": "two"}', "result two"), ('{"command": "one"}', "result one")])

    def test_an_unknown_harness_is_refused(self) -> None:
        with self.assertRaises(ValueError):
            read_steps("mystery", Path("."))


if __name__ == "__main__":
    unittest.main()
