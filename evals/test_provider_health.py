"""Checks when a provider counts as too slow for a fair run, and which trials met a provider error."""

from datetime import datetime, timezone
import json
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest import mock
import urllib.error

from evals.provider_health import (
    CacheUse,
    Endpoint,
    cache_problem,
    check,
    finished_job_problem,
    judge,
    met_provider_error,
    parse_endpoints,
    read_cache_use,
    trials_with_provider_errors,
)


class JudgeTests(unittest.TestCase):
    def test_the_throughput_of_the_valid_runs_is_fast_enough(self) -> None:
        # Think measured 48 to 53 tokens per second at the median and 31 to 34 in the slow tail in the valid runs.
        self.assertEqual(judge(Endpoint("DeepInfra", 48.0, 31.0)), ())

    def test_the_throughput_of_the_overloaded_hour_is_too_slow_for_both_reasons(self) -> None:
        reasons = judge(Endpoint("DeepInfra", 21.0, 3.0))
        self.assertEqual(len(reasons), 2)
        self.assertIn("median throughput is 21", reasons[0])
        self.assertIn("slow tail is 3", reasons[1])

    def test_a_fast_median_with_a_slow_tail_is_still_too_slow(self) -> None:
        self.assertEqual(len(judge(Endpoint("p", 60.0, 5.0))), 1)


class ParseTests(unittest.TestCase):
    def test_the_throughput_percentiles_of_every_endpoint_are_read(self) -> None:
        payload = {"data": {"endpoints": [{"provider_name": "DeepInfra", "throughput_last_30m": {"p50": 21, "p75": 11, "p90": 3, "p99": 1}}]}}
        self.assertEqual(parse_endpoints(payload), [Endpoint("DeepInfra", 21.0, 3.0)])

    def test_an_endpoint_without_throughput_is_an_error_and_not_taken_as_healthy(self) -> None:
        with self.assertRaisesRegex(ValueError, "reports no throughput"):
            parse_endpoints({"data": {"endpoints": [{"provider_name": "New", "throughput_last_30m": None}]}})
        with self.assertRaises(ValueError):
            parse_endpoints({"data": {"endpoints": []}})

    def test_a_reply_of_another_shape_is_a_value_error_and_not_a_key_error_that_could_escape(self) -> None:
        for payload in ({}, {"data": None}, {"data": {"endpoints": None}}, {"data": {"endpoints": ["not an object"]}}, {"error": "rate limited"}):
            with self.assertRaises(ValueError, msg=str(payload)):
                parse_endpoints(payload)  # type: ignore[arg-type]


class CheckTests(unittest.TestCase):
    def test_a_degraded_provider_gives_the_reasons_with_its_name_and_a_fast_one_gives_none(self) -> None:
        with mock.patch("evals.provider_health.fetch_endpoints", return_value=[Endpoint("DeepInfra", 20.0, 3.0)]):
            problem = check("m", "key")
        self.assertIn("DeepInfra: the median throughput is 20", problem)
        with mock.patch("evals.provider_health.fetch_endpoints", return_value=[Endpoint("DeepInfra", 50.0, 30.0)]):
            self.assertIsNone(check("m", "key"))

    def test_one_slow_provider_among_fast_ones_stops_the_run(self) -> None:
        with mock.patch("evals.provider_health.fetch_endpoints", return_value=[Endpoint("fast", 80.0, 60.0), Endpoint("slow", 10.0, 2.0)]):
            self.assertIn("slow:", check("m", "key"))

    def test_a_status_that_cannot_be_read_stops_the_run_and_is_not_taken_as_healthy(self) -> None:
        with mock.patch("evals.provider_health.fetch_endpoints", side_effect=urllib.error.URLError("no route")):
            self.assertIn("cannot be read", check("m", "key"))

    def test_a_read_timeout_or_a_refused_connection_is_reported_and_never_raised_into_the_guard(self) -> None:
        # A timeout while reading the body is a TimeoutError, which is an OSError and not a URLError.
        for error in (TimeoutError("timed out"), ConnectionResetError("reset"), ConnectionRefusedError("refused")):
            with mock.patch("evals.provider_health.fetch_endpoints", side_effect=error):
                self.assertIn("cannot be read", check("m", "key"), repr(error))


class TrialTests(unittest.TestCase):
    def job(self) -> Path:
        directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, directory, ignore_errors=True)
        return directory

    def trial(self, job: Path, name: str, harness: str, log: str) -> None:
        (job / name / "agent").mkdir(parents=True)
        (job / name / "result.json").write_text(json.dumps({"agent_info": {"name": harness}}))
        (job / name / "agent" / f"{harness}-stderr.log").write_text(log)

    def test_every_marker_of_a_provider_failure_is_found_in_the_logs_of_a_harness(self) -> None:
        for marker in (
            "stream error overloaded_error: Provider timed out after 302178ms",
            "429 rate_limit_exceeded",
            "is temporarily rate-limited upstream",
            "engine_overloaded",
            "stream error invalid_request_error: Upstream error from DeepInfra: Exception: 1 validation error",
            "Reconnecting... 1/5 (unexpected status 502 Bad Gateway: Provider returned error, url: https://openrouter.ai/api/v1/responses)",
            "API Error: 503 Service Unavailable",
            'bare: turn 96: the server answered 403: {"type":"error","error":{"type":"permission_error","message":"Key limit exceeded (total limit)."}}',
            "unexpected status 403 Forbidden: Key limit exceeded (total limit). Manage it using https://openrouter.ai/workspaces/default/keys, url: https://openrouter.ai/api/v1/responses",
            "Insufficient credits. Add more using https://openrouter.ai/settings/credits",
            "API Error: 402 This request requires more credits",
            "the server answered 401: invalid api key",
        ):
            job = self.job()
            self.trial(job, "t__a", "think", f"turn 2: {marker}")
            self.assertTrue(met_provider_error(job / "t__a"), marker)

    def test_a_rejection_that_the_provider_repeats_for_the_same_request_is_not_a_provider_failure(self) -> None:
        for text in (
            "Reconnecting... 1/5 (unexpected status 422 Unprocessable Entity: Provider returned error, url: https://openrouter.ai/api/v1/responses)",
            "API Error: 422 Input should be a valid string",
        ):
            job = self.job()
            self.trial(job, "t__a", "codex", text)
            self.assertFalse(met_provider_error(job / "t__a"), text)

    def test_an_ordinary_log_is_clean_and_the_trials_are_listed_by_harness(self) -> None:
        job = self.job()
        self.trial(job, "a__1", "think", "turn 1 done\nturn 2 done")
        self.trial(job, "b__2", "bare", "stream error overloaded_error")
        self.trial(job, "c__3", "bare", "stream error overloaded_error")
        self.assertFalse(met_provider_error(job / "a__1"))
        self.assertEqual(trials_with_provider_errors(job), {"bare": ["b__2", "c__3"]})

    def test_a_trial_that_harbor_ended_with_a_rate_limit_error_met_a_provider_error_even_with_clean_logs(self) -> None:
        job = self.job()
        self.trial(job, "d__4", "codex", "turn 1 done")
        (job / "d__4" / "result.json").write_text(json.dumps({"agent_info": {"name": "codex"}, "exception_info": {"exception_type": "ApiRateLimitError", "exception_message": ""}}))
        self.assertTrue(met_provider_error(job / "d__4"))


class CacheUseTests(unittest.TestCase):
    def job(self, usage: list[tuple[int | None, int | None]]) -> Path:
        directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, directory, ignore_errors=True)
        for index, (counted, cached) in enumerate(usage):
            (directory / f"t{index}__x").mkdir()
            result = {"agent_info": {"name": "think"}, "agent_result": {"n_input_tokens": counted, "n_cache_tokens": cached}}
            (directory / f"t{index}__x" / "result.json").write_text(json.dumps(result))
        return directory

    def test_the_ratio_weighs_each_trial_by_its_input_tokens_and_does_not_average_the_ratios(self) -> None:
        # A short trial at 50% and a long one at 100%: 1,000 of 2,000 and 98,000 of 98,000 is 99%.
        use = read_cache_use([self.job([(2_000, 1_000), (98_000, 98_000)])])
        self.assertEqual((use.trials, use.input_tokens, use.cached_tokens), (2, 100_000, 99_000))
        self.assertAlmostEqual(use.hit_ratio, 0.99)

    def test_a_trial_that_reported_no_usage_counts_for_nothing_and_not_as_a_trial_without_cache_hits(self) -> None:
        use = read_cache_use([self.job([(None, None), (0, 0), (1_000, None), (1_000, 900)])])
        self.assertEqual((use.trials, use.hit_ratio), (1, 0.9))

    def test_a_job_without_trials_has_no_ratio(self) -> None:
        self.assertIsNone(read_cache_use([self.job([])]).hit_ratio)

    def test_the_jobs_are_added_together(self) -> None:
        use = read_cache_use([self.job([(1_000, 1_000)]), self.job([(1_000, 0)])])
        self.assertEqual((use.trials, use.hit_ratio), (2, 0.5))

    def test_with_a_since_time_only_the_trials_that_finished_at_or_after_it_count(self) -> None:
        directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, directory, ignore_errors=True)
        for name, finished, counted, cached in (("old__x", "2026-10-20T03:00:00.000000Z", 1_000, 1_000), ("new__x", "2026-10-20T04:00:00.000000Z", 1_000, 500), ("none__x", None, 1_000, 0)):
            (directory / name).mkdir()
            result = {"agent_info": {"name": "think"}, "finished_at": finished, "agent_result": {"n_input_tokens": counted, "n_cache_tokens": cached}}
            (directory / name / "result.json").write_text(json.dumps(result))
        since = datetime(2026, 10, 20, 3, 30, tzinfo=timezone.utc)
        use = read_cache_use([directory], since=since)
        self.assertEqual((use.trials, use.hit_ratio), (1, 0.5))
        # Without since, every trial counts, whatever its finish time.
        self.assertEqual(read_cache_use([directory]).trials, 3)


class CacheProblemTests(unittest.TestCase):
    def test_a_running_job_under_the_limit_is_a_problem_once_enough_trials_have_finished(self) -> None:
        # The two stopped jobs of October 19 were at 83.2% and 86.0%; the running limit is 92%.
        self.assertIn("83.2%", cache_problem(CacheUse(46, 1_000_000, 832_000), minimum_ratio=0.92))
        self.assertIsNone(cache_problem(CacheUse(46, 1_000_000, 973_000), minimum_ratio=0.92))

    def test_too_few_finished_trials_do_not_tell_and_are_not_a_problem(self) -> None:
        self.assertIsNone(cache_problem(CacheUse(19, 1_000_000, 500_000), minimum_ratio=0.92))
        self.assertIn("50.0%", cache_problem(CacheUse(20, 1_000_000, 500_000), minimum_ratio=0.92))

    def test_the_limit_itself_is_not_a_problem(self) -> None:
        self.assertIsNone(cache_problem(CacheUse(100, 1_000_000, 920_000), minimum_ratio=0.92))

    def test_a_finished_job_is_judged_on_every_trial_and_a_job_without_usage_is_a_problem(self) -> None:
        # The valid jobs ended at 97.2% to 97.5%; the limit for a finished job is 95%.
        self.assertIsNone(finished_job_problem(CacheUse(190, 1_000_000, 973_000)))
        self.assertIn("93.0%", finished_job_problem(CacheUse(190, 1_000_000, 930_000)))
        # Sixteen trials are too few for a running job but they are the whole of a finished one.
        self.assertIn("51.1%", finished_job_problem(CacheUse(16, 1_000_000, 511_000)))
        self.assertIn("unknown", finished_job_problem(CacheUse(0, 0, 0)))


if __name__ == "__main__":
    unittest.main()
