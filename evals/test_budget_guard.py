"""Host-side checks for controller process state."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from evals.budget_guard import _alive, modal_session_prefixes


class ProcessStateTests(unittest.TestCase):
    def test_exited_child_is_not_alive_before_reaping(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            pid_file = Path(directory) / "controller.pid"
            with subprocess.Popen(
                [sys.executable, "-c", "raise SystemExit(0)"],
                stdout=subprocess.PIPE,
            ) as process:
                pid_file.write_text(str(process.pid))
                assert process.stdout is not None
                process.stdout.read()
                self.assertFalse(_alive(pid_file))

    def test_waiting_child_is_alive(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            pid_file = Path(directory) / "controller.pid"
            with subprocess.Popen(
                [sys.executable, "-c", "import sys; sys.stdin.read()"],
                stdin=subprocess.PIPE,
            ) as process:
                try:
                    pid_file.write_text(str(process.pid))
                    self.assertTrue(_alive(pid_file))
                finally:
                    assert process.stdin is not None
                    process.stdin.close()


class ModalSandboxOwnershipTests(unittest.TestCase):
    def test_only_saved_trials_of_the_watched_jobs_own_sandboxes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            job = Path(directory) / "job"
            for trial in ("regex-log__abc", "fix-git__xyz"):
                (job / trial).mkdir(parents=True)
                (job / trial / "config.json").write_text("{}")
            (job / "stray-folder").mkdir()
            (job / "config.json").write_text("{}")
            prefixes = modal_session_prefixes([job, Path(directory) / "missing"])
            self.assertEqual(sorted(prefixes), ["fix-git__xyz__", "regex-log__abc__"])

    def test_a_trial_does_not_own_the_sandbox_of_a_trial_with_a_longer_name(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            job = Path(directory)
            (job / "regex-log__abc").mkdir()
            (job / "regex-log__abc" / "config.json").write_text("{}")
            prefixes = modal_session_prefixes([job])
            self.assertTrue("regex-log__abc__env".startswith(prefixes))
            self.assertTrue("regex-log__abc__verifier__main".startswith(prefixes))
            self.assertFalse("regex-log__abcd__env".startswith(prefixes))
