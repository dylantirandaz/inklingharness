"""Checks which trials the fault rule archives and which it keeps."""

import json
from pathlib import Path
import shutil
import tempfile
import unittest

from evals.trial_faults import Counted, Fault, Unclassified, archive, judge_job, judge_trial, main

PASS = {"rewards": {"reward": 1.0}}
FAIL = {"rewards": {"reward": 0.0}}


class TrialFaultTests(unittest.TestCase):
    def setUp(self) -> None:
        self.job = Path(tempfile.mkdtemp()) / "job-a"
        self.job.mkdir()
        self.addCleanup(shutil.rmtree, self.job.parent, ignore_errors=True)

    def trial(self, name: str, *, exception: tuple[str, str] | None = None, verifier: dict | None = PASS, log: str = "turn 1 done", harness: str = "bare") -> Path:
        directory = self.job / f"{name}__x{len(list(self.job.iterdir()))}"
        (directory / "agent").mkdir(parents=True)
        result = {"task_name": f"org/{name}", "agent_info": {"name": harness}, "verifier_result": verifier}
        if exception is not None:
            result["exception_info"] = {"exception_type": exception[0], "exception_message": exception[1]}
        (directory / "result.json").write_text(json.dumps(result))
        (directory / "agent" / f"{harness}-stderr.log").write_text(log)
        return directory

    def test_a_graded_trial_that_ended_without_an_exception_counts(self) -> None:
        self.assertEqual(judge_trial(self.trial("clean")), Counted())

    def test_the_ends_that_the_agent_takes_part_in_count_with_their_grade(self) -> None:
        for kind, message in (
            ("AgentTimeoutError", "Agent execution timed out after 3000 seconds"),
            ("NonZeroAgentExitCodeError", "Command failed (exit 1): /installed-agent/bare"),
            ("NonZeroAgentExitCodeError", "Command failed (exit 137): claude"),
            ("UnknownApiError", "Command failed (exit 1): claude ... API Error: 422 Input should be a valid string"),
        ):
            self.assertEqual(judge_trial(self.trial("t", exception=(kind, message), verifier=FAIL)), Counted(), kind + message)

    def test_a_request_that_the_provider_rejects_every_time_counts_and_a_failure_of_the_provider_does_not(self) -> None:
        rejected = self.trial("a", exception=("NonZeroAgentExitCodeError", "Command failed (exit 1): codex"), verifier=FAIL, harness="codex", log="Reconnecting... 5/5 (unexpected status 422 Unprocessable Entity: Provider returned error)")
        failed = self.trial("b", exception=("NonZeroAgentExitCodeError", "Command failed (exit 1): bare"), log="bare: turn 39: stream error invalid_request_error: Upstream error from DeepInfra: Exception")
        self.assertEqual(judge_trial(rejected), Counted())
        self.assertEqual(judge_trial(failed), Fault("the provider failed during the trial"))

    def test_a_harness_that_retried_after_a_provider_failure_and_finished_is_a_fault_too(self) -> None:
        self.assertEqual(judge_trial(self.trial("c", log="retry after: Upstream error from DeepInfra\nturn 5 done")), Fault("the provider failed during the trial"))

    def test_a_trial_that_ended_on_a_key_limit_without_any_exception_is_a_fault(self) -> None:
        # Pi ends with exit 0 after a 403, and the sandbox then grades the unfinished work.
        trial = self.trial("p", verifier=FAIL, harness="pi", log='error: 403 {"message":"Key limit exceeded (total limit). $500.05 of $500.00 used"}')
        self.assertEqual(judge_trial(trial), Fault("the provider failed during the trial"))

    def test_the_setup_failures_and_the_sigterm_are_faults_whatever_the_grade(self) -> None:
        for exception, verifier in (
            (("AgentSetupTimeoutError", "Agent setup timed out after 360.0 seconds"), None),
            (("AlreadyExistsError", "Sandbox with name 'x' already exists"), None),
            (("RuntimeError", "Agent install failed: 'litellm'"), None),
            (("NonZeroAgentExitCodeError", "Command failed (exit 143): /installed-agent/bare"), FAIL),
        ):
            self.assertIsInstance(judge_trial(self.trial("d", exception=exception, verifier=verifier)), Fault, exception)

    def test_an_exception_that_the_rule_does_not_know_and_a_trial_without_a_grade_are_unclassified(self) -> None:
        for trial in (
            self.trial("e", exception=("ContainerExploded", "boom"), verifier=FAIL),
            self.trial("f", exception=("RuntimeError", "something else"), verifier=FAIL),
            self.trial("g", verifier=None),
        ):
            self.assertIsInstance(judge_trial(trial), Unclassified)

    def test_archiving_moves_only_the_faults_and_records_why(self) -> None:
        self.trial("kept")
        fault = self.trial("lost", exception=("AgentSetupTimeoutError", "Agent setup timed out after 360.0 seconds"), verifier=None, harness="think")
        unknown = self.trial("unknown", exception=("ContainerExploded", "boom"), verifier=FAIL)
        directory = self.job.parent / "archive"
        judged = judge_job(self.job)
        archive([item for item in judged if isinstance(item.verdict, Fault)], directory)
        self.assertFalse(fault.exists())
        self.assertTrue(unknown.exists())
        self.assertEqual(len(list(self.job.glob("*__*/result.json"))), 2)
        self.assertTrue((directory / "job-a" / fault.name / "result.json").exists())
        manifest = json.loads((directory / "manifest.json").read_text())
        self.assertEqual(manifest, [{"job": "job-a", "trial": fault.name, "harness": "think", "task": "lost", "grade": None, "reason": "the sandbox did not set the agent up in time"}])

    def test_the_command_exits_zero_only_when_every_trial_counts(self) -> None:
        self.trial("kept")
        self.assertEqual(main([str(self.job)]), 0)
        self.trial("lost", exception=("AgentSetupTimeoutError", "Agent setup timed out after 360.0 seconds"), verifier=None)
        self.assertEqual(main([str(self.job)]), 1)


if __name__ == "__main__":
    unittest.main()
