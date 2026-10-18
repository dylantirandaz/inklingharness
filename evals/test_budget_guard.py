"""Host-side checks for controller process state."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from evals.budget_guard import _alive


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
