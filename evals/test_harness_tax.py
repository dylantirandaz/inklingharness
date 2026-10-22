"""Checks the HarnessTax measures against small cases that can be worked by hand."""

import json
import math
from pathlib import Path
import shutil
import tempfile
import unittest

from evals.harness_tax import (
    Rollout,
    analyze,
    bootstrap_interval,
    cumulative_curve,
    frontier_frequencies,
    harness_effects,
    pareto_frontier,
    ratio_interval,
    read_transcript,
    task_means,
    tasks_to_resolve_ratio,
    tasks_to_resolve_success,
)


def rollout(passed: bool, dollars: float | None = 1.0, turns: int | None = 10, tokens: int | None = 100, first: int | None = 50) -> Rollout:
    return Rollout(passed=passed, dollars=dollars, tokens=tokens, turns=turns, first_call_tokens=first)


class ParetoFrontierTests(unittest.TestCase):
    def test_a_harness_that_another_beats_on_both_axes_is_off_the_frontier(self) -> None:
        points = {"cheap and good": (1.0, 0.9), "dear and poor": (2.0, 0.5)}
        self.assertEqual(pareto_frontier(points), {"cheap and good"})

    def test_a_cheaper_harness_and_a_better_one_both_stay(self) -> None:
        points = {"cheap": (1.0, 0.5), "good": (2.0, 0.9)}
        self.assertEqual(pareto_frontier(points), {"cheap", "good"})

    def test_the_same_cost_with_less_success_is_beaten_and_a_full_tie_is_not(self) -> None:
        self.assertEqual(pareto_frontier({"a": (1.0, 0.9), "b": (1.0, 0.5)}), {"a"})
        self.assertEqual(pareto_frontier({"a": (1.0, 0.9), "b": (1.0, 0.9)}), {"a", "b"})


class CumulativeCurveTests(unittest.TestCase):
    # Four attempts. Sorted by cost: 1 (pass), 2 (fail), 3 (pass), 4 (fail). The cost per attempt grows by
    # 0.25, 0.5, 0.75, and 1.0, and the success rate by 0.25, 0, 0.25, and 0.
    CURVE = cumulative_curve([(3.0, True), (1.0, True), (4.0, False), (2.0, False)])

    def test_the_points_follow_the_cheapest_attempts_first(self) -> None:
        self.assertEqual(self.CURVE.x, (0.0, 0.25, 0.75, 1.5, 2.5))
        self.assertEqual(self.CURVE.y, (0.0, 0.25, 0.25, 0.5, 0.5))

    def test_the_last_point_is_the_mean_cost_and_the_success_rate(self) -> None:
        self.assertEqual((self.CURVE.x[-1], self.CURVE.y[-1]), (10 / 4, 2 / 4))

    def test_a_failed_attempt_adds_cost_without_success_and_the_curve_is_linear_between_points(self) -> None:
        self.assertAlmostEqual(self.CURVE.success_within(0.5), 0.25)
        # Halfway along the third segment, from (0.75, 0.25) to (1.5, 0.5).
        self.assertAlmostEqual(self.CURVE.success_within(1.125), 0.375)

    def test_it_is_zero_without_a_budget_and_level_past_the_end(self) -> None:
        self.assertEqual(self.CURVE.success_within(0.0), 0.0)
        self.assertEqual(self.CURVE.success_within(100.0), 0.5)

    def test_attempts_with_equal_cost_are_added_together(self) -> None:
        curve = cumulative_curve([(2.0, True), (2.0, False), (6.0, True)])
        self.assertEqual(curve.x, (0.0, 4 / 3, 10 / 3))
        self.assertEqual(curve.y, (0.0, 1 / 3, 2 / 3))

    def test_no_attempt_gives_no_curve(self) -> None:
        with self.assertRaises(ValueError):
            cumulative_curve([])


class IntervalTests(unittest.TestCase):
    def test_equal_values_have_no_spread(self) -> None:
        self.assertEqual(bootstrap_interval([0.5] * 8, resamples=500), (0.5, 0.5))

    def test_the_interval_surrounds_the_mean_and_repeats_with_the_same_seed(self) -> None:
        values = [0, 1, 1, 0, 1, 1, 1, 0, 1, 1]
        low, high = bootstrap_interval(values, resamples=2000)
        self.assertLess(low, 0.7)
        self.assertGreater(high, 0.7)
        self.assertEqual((low, high), bootstrap_interval(values, resamples=2000))

    def test_a_ratio_that_holds_on_every_task_has_no_spread(self) -> None:
        self.assertEqual(ratio_interval([2.0, 6.0, 4.0], [1.0, 3.0, 2.0], resamples=500), (2.0, 2.0, 2.0))

    def test_the_ratio_is_the_ratio_of_means_and_not_the_mean_of_ratios(self) -> None:
        # Tasks of cost (1, 1) and (9, 3): the means are 5 and 2, so the ratio is 2.5. The mean of the ratios is 2.
        point, low, high = ratio_interval([1.0, 9.0], [1.0, 3.0], resamples=500)
        self.assertEqual(point, 2.5)
        self.assertLessEqual(low, point)
        self.assertGreaterEqual(high, point)


class FrontierFrequencyTests(unittest.TestCase):
    def test_a_harness_that_wins_on_every_task_is_on_the_frontier_in_every_resample(self) -> None:
        shares = frontier_frequencies({"a": [1.0, 1.0, 1.0], "b": [2.0, 2.0, 2.0]}, {"a": [1.0, 1.0, 1.0], "b": [0.0, 0.0, 0.0]}, resamples=300)
        self.assertEqual(shares, {"a": 1.0, "b": 0.0})

    def test_a_cheaper_harness_and_a_better_one_share_the_frontier(self) -> None:
        shares = frontier_frequencies({"a": [1.0, 1.0], "b": [2.0, 2.0]}, {"a": [0.0, 0.0], "b": [1.0, 1.0]}, resamples=300)
        self.assertEqual(shares, {"a": 1.0, "b": 1.0})


class HarnessEffectTests(unittest.TestCase):
    def test_the_effects_are_the_distance_from_the_mean_and_sum_to_zero(self) -> None:
        effects = harness_effects({"a": 0.9, "b": 0.6, "c": 0.3})
        self.assertAlmostEqual(effects["a"], 0.3)
        self.assertAlmostEqual(effects["c"], -0.3)
        self.assertAlmostEqual(sum(effects.values()), 0.0)


class TaskMeansTests(unittest.TestCase):
    def test_a_task_averages_its_attempts_that_have_the_measure_and_is_left_out_when_none_has_it(self) -> None:
        arm = {"x": (rollout(True, dollars=1.0), rollout(False, dollars=3.0), rollout(False, dollars=None)), "y": (rollout(True, dollars=None),)}
        self.assertEqual(task_means(arm, lambda r: r.dollars), {"x": 2.0})


class ResolutionTests(unittest.TestCase):
    def test_the_tasks_follow_the_spread_of_the_differences_and_the_width_that_is_asked_for(self) -> None:
        # Differences of +-0.2 have a sample spread a little above 0.2, so a two-point interval needs about (1.96 * 0.2 / 0.02) ** 2 = 384 tasks.
        differences = [0.2, -0.2] * 50
        spread = (sum(d * d for d in differences) / (len(differences) - 1)) ** 0.5
        self.assertEqual(tasks_to_resolve_success(differences, 0.02), math.ceil((1.96 * spread / 0.02) ** 2))
        self.assertGreaterEqual(tasks_to_resolve_success(differences, 0.02), 384)
        # Four differences of +-0.2 have a sample spread of sqrt(4 * 0.04 / 3) = 0.2309, so (1.96 * 0.2309 / 0.2) ** 2 = 5.12 asks for 6 tasks.
        self.assertEqual(tasks_to_resolve_success([0.2, -0.2, 0.2, -0.2], 0.2), 6)

    def test_a_ratio_interval_that_is_already_5_percent_wide_needs_the_tasks_that_it_has_and_a_wider_one_needs_more(self) -> None:
        exactly = (1.0, 1 / 1.05, 1.05)
        self.assertEqual(tasks_to_resolve_ratio(100, exactly, 5.0), 100)
        wider = (1.0, 1 / 1.10, 1.10)
        # Twice the half-width on the log scale needs about four times the tasks.
        self.assertGreater(tasks_to_resolve_ratio(100, wider, 5.0), 380)
        self.assertLess(tasks_to_resolve_ratio(100, wider, 5.0), 410)


class TranscriptTests(unittest.TestCase):
    def folder(self) -> Path:
        directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, directory, ignore_errors=True)
        (directory / "agent").mkdir()
        return directory

    def test_think_counts_its_turns_and_reads_the_first_call_with_cached_tokens(self) -> None:
        directory = self.folder()
        events = [
            {"type": "turn", "turn": 1, "usage": {"input_tokens": 112, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 1664, "output_tokens": 45}},
            {"type": "tool_call", "name": "bash"},
            {"type": "turn", "turn": 2, "usage": {"input_tokens": 50, "cache_read_input_tokens": 1776, "output_tokens": 10}},
        ]
        (directory / "agent" / "think-events.jsonl").write_text("\n".join(json.dumps(event) for event in events) + "\nnot json\n")
        transcript = read_transcript("think", directory)
        self.assertEqual((transcript.turns, transcript.first_call_tokens), (2, 112 + 1664))

    def test_the_baseline_may_report_null_cache_fields(self) -> None:
        directory = self.folder()
        events = [{"event": "turn", "usage": {"input_tokens": 318, "cache_creation_input_tokens": None, "cache_read_input_tokens": 0}}, {"event": "end", "turns": 1}]
        (directory / "agent" / "bare-events.jsonl").write_text("\n".join(json.dumps(event) for event in events))
        transcript = read_transcript("bare", directory)
        self.assertEqual((transcript.turns, transcript.first_call_tokens), (1, 318))

    def test_atif_counts_the_model_calls_of_the_agent_steps_and_not_the_user_steps(self) -> None:
        directory = self.folder()
        steps = [
            {"source": "user", "message": "task"},
            {"source": "agent", "llm_call_count": 1, "metrics": {"prompt_tokens": 9222}},
            {"source": "agent", "llm_call_count": 2, "metrics": {"prompt_tokens": 9300}},
            {"source": "agent", "metrics": {"prompt_tokens": 9400}},
        ]
        (directory / "agent" / "trajectory.json").write_text(json.dumps({"steps": steps}))
        transcript = read_transcript("codex", directory)
        self.assertEqual((transcript.turns, transcript.first_call_tokens), (4, 9222))

    def test_a_harness_without_a_reader_is_refused_and_a_missing_transcript_is_reported(self) -> None:
        with self.assertRaises(ValueError):
            read_transcript("mystery", Path("."))
        with self.assertRaises(FileNotFoundError):
            read_transcript("pi", self.folder())


class AnalyzeTests(unittest.TestCase):
    # Two tasks, two attempts each. "good" passes everything at $1 and 10 turns an attempt.
    # "dear" passes half of its attempts at $2 and 20 turns an attempt.
    ARMS = {
        "good": {"t1": (rollout(True), rollout(True)), "t2": (rollout(True), rollout(True))},
        "dear": {"t1": (rollout(True, 2.0, 20), rollout(False, 2.0, 20)), "t2": (rollout(False, 2.0, 20), rollout(True, 2.0, 20))},
    }

    def test_success_cost_and_the_frontier_follow_the_definitions(self) -> None:
        report = analyze(self.ARMS, reference="good", own="good", resamples=200)
        rows = report["harnesses"]
        self.assertEqual((rows["good"]["success"], rows["dear"]["success"]), (1.0, 0.5))
        self.assertEqual((rows["good"]["dollars_per_attempt"], rows["dear"]["dollars_per_attempt"]), (1.0, 2.0))
        self.assertEqual(report["pareto_frontier"], ["good"])
        self.assertEqual(report["cost_ratio_to_reference"]["dear"], (2.0, 2.0, 2.0))
        self.assertEqual(report["harness_effect_on_success"], {"good": 0.25, "dear": -0.25})
        self.assertEqual((report["highest_success"], report["lowest_cost"]), (["good"], ["good"]))
        self.assertEqual(report["own_harness"], {"label": "good", "has_highest_success": True, "has_lowest_cost": True})

    def test_cost_per_turn_and_the_turn_cap_are_counted_over_attempts(self) -> None:
        arms = {"a": {"t": (rollout(True, 1.0, 50), rollout(True, 3.0, 150))}}
        row = analyze(arms, reference="a", resamples=100)["harnesses"]["a"]
        self.assertEqual(row["dollars_per_turn"], 4.0 / 200)
        self.assertEqual((row["most_turns_in_an_attempt"], row["attempts_over_turn_cap"]), (150, 1))

    def test_the_curves_are_read_at_multiples_of_the_lowest_mean_spend(self) -> None:
        report = analyze(self.ARMS, reference="good", resamples=100)
        dollars = report["cumulative_curves"]["dollars"]
        self.assertEqual(dollars["budgets"], [0.5, 0.75, 1.0, 1.25, 1.5])
        self.assertEqual(dollars["success_within_budget"]["good"], [0.5, 0.75, 1.0, 1.0, 1.0])
        # "dear" has one group of four attempts at $2, so its curve is the line from (0, 0) to (2, 0.5).
        self.assertEqual(dollars["success_within_budget"]["dear"], [0.5 * b / 2 for b in (0.5, 0.75, 1.0, 1.25, 1.5)])

    def test_a_task_without_a_cost_for_every_harness_leaves_the_cost_measures_and_stays_in_success(self) -> None:
        arms = {
            "a": {"t1": (rollout(True, 1.0),), "t2": (rollout(True, None),)},
            "b": {"t1": (rollout(False, 2.0),), "t2": (rollout(False, 2.0),)},
        }
        report = analyze(arms, reference="a", resamples=100)
        self.assertEqual((report["tasks"], report["tasks_with_cost_for_every_harness"]), (2, 1))
        self.assertEqual(report["harnesses"]["a"]["success"], 1.0)
        self.assertEqual(report["harnesses"]["a"]["dollars_per_attempt"], 1.0)
        self.assertEqual(report["harnesses"]["a"]["attempts_without_usage"], 1)

    def test_arms_with_different_tasks_and_an_unknown_reference_are_refused(self) -> None:
        with self.assertRaises(ValueError):
            analyze({"a": {"t1": (rollout(True),)}, "b": {"t2": (rollout(True),)}}, reference="a", resamples=10)
        with self.assertRaises(ValueError):
            analyze(self.ARMS, reference="missing", resamples=10)


if __name__ == "__main__":
    unittest.main()
