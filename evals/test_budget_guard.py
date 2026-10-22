"""Host-side checks for controller process state."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

from evals.budget_guard import _alive, _retry, funds_problem, modal_sandboxes, modal_session_prefixes, next_degraded_count


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

    def trial_folder(self, directory: str) -> Path:
        job = Path(directory)
        (job / "regex-log__abc").mkdir(exist_ok=True)
        (job / "regex-log__abc" / "config.json").write_text("{}")
        return job

    def test_a_lost_connection_to_modal_is_an_oserror_so_that_the_loop_retries_it_and_stops_the_jobs_when_it_keeps_failing(self) -> None:
        import grpclib.exceptions
        import modal

        for error in (grpclib.exceptions.StreamTerminatedError("Connection lost"), modal.exception.ConnectionError("no route")):
            with tempfile.TemporaryDirectory() as directory, mock.patch.object(modal.App, "lookup", side_effect=error):
                with self.assertRaises(OSError):
                    modal_sandboxes([self.trial_folder(directory)], "app")

    def test_the_listing_is_retried_after_a_lost_connection_and_returns_what_the_next_try_reads(self) -> None:
        import grpclib.exceptions
        import modal

        calls = []

        def lookup(*args, **kwargs):
            calls.append(1)
            if len(calls) == 1:
                raise grpclib.exceptions.StreamTerminatedError("Connection lost")
            raise modal.exception.NotFoundError("no app yet")

        with tempfile.TemporaryDirectory() as directory, mock.patch.object(modal.App, "lookup", side_effect=lookup):
            owned = _retry(lambda: modal_sandboxes([self.trial_folder(directory)], "app"), attempts=3, pause=0.0)
        self.assertEqual((owned, len(calls)), ([], 2))

    def test_a_modal_app_that_does_not_exist_yet_means_no_sandboxes_and_is_not_an_error(self) -> None:
        import modal

        with tempfile.TemporaryDirectory() as directory, mock.patch.object(modal.App, "lookup", side_effect=modal.exception.NotFoundError("no app yet")):
            self.assertEqual(modal_sandboxes([self.trial_folder(directory)], "app"), [])


class FundsTests(unittest.TestCase):
    def test_credits_and_key_limit_that_both_hold_the_minimum_are_enough(self) -> None:
        self.assertIsNone(funds_problem(credits_left=14.71, key_limit_left=9.0, minimum=6.0))

    def test_a_key_without_a_limit_of_its_own_is_judged_on_the_credits_alone(self) -> None:
        self.assertIsNone(funds_problem(credits_left=14.71, key_limit_left=None, minimum=6.0))
        self.assertEqual(funds_problem(credits_left=3.0, key_limit_left=None, minimum=6.0), "OpenRouter credits exhausted")

    def test_a_key_limit_under_the_minimum_stops_a_run_even_when_the_account_has_credits(self) -> None:
        self.assertEqual(funds_problem(credits_left=14.71, key_limit_left=0.0, minimum=6.0), "the limit of the OpenRouter key is exhausted")

    def test_empty_credits_are_reported_first_when_both_are_short(self) -> None:
        self.assertEqual(funds_problem(credits_left=1.0, key_limit_left=0.0, minimum=6.0), "OpenRouter credits exhausted")


class DegradedProviderTests(unittest.TestCase):
    def test_the_count_grows_with_each_degraded_check_in_a_row(self) -> None:
        count = 0
        for _ in range(3):
            count = next_degraded_count("DeepInfra: the median throughput is 20", count)
        self.assertEqual(count, 3)

    def test_one_healthy_check_ends_the_row_so_that_a_short_slowdown_does_not_stop_a_run(self) -> None:
        count = 0
        for problem in ("slow", "slow", None, "slow"):
            count = next_degraded_count(problem, count)
        self.assertEqual(count, 1)


class RetryTests(unittest.TestCase):
    def test_a_read_that_fails_twice_and_then_succeeds_returns_its_value_after_three_calls(self) -> None:
        calls = []

        def read() -> float:
            calls.append(1)
            if len(calls) < 3:
                raise ConnectionResetError("reset by peer")
            return 4.2

        self.assertEqual(_retry(read, attempts=5, pause=0.0), 4.2)
        self.assertEqual(len(calls), 3)

    def test_a_read_that_always_fails_is_tried_every_attempt_and_then_raised_as_an_outage(self) -> None:
        calls = []

        def read() -> float:
            calls.append(1)
            raise ConnectionResetError("down")

        with self.assertRaises(ConnectionResetError):
            _retry(read, attempts=4, pause=0.0)
        self.assertEqual(len(calls), 4)

    def test_an_error_that_is_not_a_read_failure_is_not_retried(self) -> None:
        calls = []

        def read() -> float:
            calls.append(1)
            raise KeyError("a bug in the guard")

        with self.assertRaises(KeyError):
            _retry(read, attempts=4, pause=0.0)
        self.assertEqual(len(calls), 1)
