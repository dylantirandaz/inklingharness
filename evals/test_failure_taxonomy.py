"""Checks how a trial is put into a kind."""

import json
from pathlib import Path
import shutil
import tempfile
import unittest

from evals.failure_taxonomy import KINDS, Classified, classify_job, classify_trial, table

TOOL_CALL_TEXT = '{"type":"text","text":"{\\"name\\":\\"Read\\",\\"args\\":{\\"file_path\\":\\"/testbed/a.py\\",\\"pages\\":\\"}}Bash{\\""}}'


class ClassifyTests(unittest.TestCase):
    def setUp(self) -> None:
        self.job = Path(tempfile.mkdtemp()) / "job"
        self.job.mkdir()
        self.addCleanup(shutil.rmtree, self.job.parent, ignore_errors=True)
        self.count = 0

    def trial(self, *, reward: float | None = 0.0, exception: tuple[str, str] | None = None, log: str = "", steps: list[tuple[str, str]] | None = None, report: dict | None = None) -> Path:
        self.count += 1
        directory = self.job / f"task{self.count}__x{self.count}"
        (directory / "agent").mkdir(parents=True)
        result = {"task_name": f"org/task{self.count}", "agent_info": {"name": "think"}, "verifier_result": None if reward is None else {"rewards": {"reward": reward}}}
        if exception is not None:
            result["exception_info"] = {"exception_type": exception[0], "exception_message": exception[1]}
        (directory / "result.json").write_text(json.dumps(result))
        events = [line for name, arguments in (steps if steps is not None else [("bash", "ls")]) for line in (json.dumps({"type": "tool_call", "name": name, "input": arguments}), json.dumps({"type": "tool_result", "content": "ok"}))]
        (directory / "agent" / "think-events.jsonl").write_text("\n".join(events + ([log] if log else [])))
        if report is not None:
            (directory / "verifier").mkdir()
            (directory / "verifier" / "report.json").write_text(json.dumps({"task": report}))
        return directory

    def test_a_graded_one_is_passed_even_when_the_log_holds_an_error_text(self) -> None:
        self.assertEqual(classify_trial(self.trial(reward=1.0, log="unexpected status 422 Unprocessable")), "passed")

    def test_the_end_of_a_run_by_the_environment_is_a_fault_and_an_unknown_exception_is_unclassified(self) -> None:
        self.assertEqual(classify_trial(self.trial(reward=None, exception=("AgentSetupTimeoutError", "Agent setup timed out after 360.0 seconds"))), "fault")
        self.assertEqual(classify_trial(self.trial(exception=("ContainerExploded", "boom"))), "unclassified")

    def test_an_agent_that_ran_out_of_time_is_timed_out(self) -> None:
        self.assertEqual(classify_trial(self.trial(exception=("AgentTimeoutError", "Agent execution timed out after 3000 seconds"))), "timed_out")

    def test_a_log_that_ends_on_a_tool_call_written_as_text_is_that_kind_and_one_in_the_middle_of_a_run_that_went_on_is_not(self) -> None:
        self.assertEqual(classify_trial(self.trial(log=TOOL_CALL_TEXT)), "tool_call_as_text")
        recovered = self.trial(steps=[("edit_file", '{"path":"a.py"}')], log=TOOL_CALL_TEXT + "\n" + "x" * 9000, report={"tests_status": {"FAIL_TO_PASS": {"failure": ["t"]}, "PASS_TO_PASS": {"failure": []}}})
        self.assertEqual(classify_trial(recovered), "wrong_fix")

    def test_a_422_at_the_end_is_provider_rejected(self) -> None:
        self.assertEqual(classify_trial(self.trial(log="Reconnecting... 5/5 (unexpected status 422 Unprocessable Entity: Provider returned error)")), "provider_rejected")

    def test_an_agent_that_only_read_files_made_no_edit_and_one_that_edited_did(self) -> None:
        self.assertEqual(classify_trial(self.trial(steps=[("read_file", '{"path":"a.py"}'), ("bash", "grep -rn foo .")])), "no_edit")
        self.assertEqual(classify_trial(self.trial(steps=[("edit_file", '{"path":"a.py"}')])), "failed")
        self.assertEqual(classify_trial(self.trial(steps=[("bash", "sed -i 's/a/b/' a.py")])), "failed")

    def test_a_swe_bench_report_tells_a_wrong_fix_from_a_regression(self) -> None:
        edit = [("edit_file", '{"path":"a.py"}')]
        wrong = {"tests_status": {"FAIL_TO_PASS": {"failure": ["t1"]}, "PASS_TO_PASS": {"failure": []}}}
        broke = {"tests_status": {"FAIL_TO_PASS": {"failure": []}, "PASS_TO_PASS": {"failure": ["t2"]}}}
        both = {"tests_status": {"FAIL_TO_PASS": {"failure": ["t1"]}, "PASS_TO_PASS": {"failure": ["t2"]}}}
        self.assertEqual(classify_trial(self.trial(steps=edit, report=wrong)), "wrong_fix")
        self.assertEqual(classify_trial(self.trial(steps=edit, report=broke)), "broke_other_tests")
        self.assertEqual(classify_trial(self.trial(steps=edit, report=both)), "broke_other_tests")


class TableTests(unittest.TestCase):
    def test_the_table_counts_by_harness_and_shows_only_the_kinds_that_occur(self) -> None:
        items = [Classified(Path("j"), Path("j/a__1"), "think", "a", "passed"), Classified(Path("j"), Path("j/b__2"), "think", "b", "wrong_fix"), Classified(Path("j"), Path("j/c__3"), "pi", "c", "passed")]
        lines = table(items).splitlines()
        self.assertEqual(len(lines), 3)
        self.assertIn("wrong_fix", lines[0])
        self.assertNotIn("timed_out", lines[0])
        self.assertTrue(lines[1].startswith("pi") and lines[2].startswith("think"))

    def test_every_kind_is_unique(self) -> None:
        self.assertEqual(len(set(KINDS)), len(KINDS))

    def test_a_job_with_no_trial_gives_no_row(self) -> None:
        empty = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, empty, ignore_errors=True)
        self.assertEqual(classify_job(empty), [])


if __name__ == "__main__":
    unittest.main()
