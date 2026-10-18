"""Checks of the interval, the paired test, and the arm specification."""

from pathlib import Path
import unittest

from evals.compare_arms import Outcome, dollars, efficiency, exact_mcnemar, parse_arm, wilson_interval, Z95


class WilsonIntervalTests(unittest.TestCase):
    def test_no_passes_end_at_the_closed_form_bound(self) -> None:
        low, high = wilson_interval(0, 10)
        self.assertEqual(low, 0.0)
        self.assertAlmostEqual(high, Z95**2 / (10 + Z95**2))

    def test_all_passes_start_at_the_closed_form_bound(self) -> None:
        low, high = wilson_interval(10, 10)
        self.assertAlmostEqual(low, 10 / (10 + Z95**2))
        self.assertEqual(high, 1.0)

    def test_interval_mirrors_when_passes_and_failures_swap(self) -> None:
        low, high = wilson_interval(47, 89)
        mirrored_low, mirrored_high = wilson_interval(89 - 47, 89)
        self.assertAlmostEqual(low, 1 - mirrored_high)
        self.assertAlmostEqual(high, 1 - mirrored_low)
        self.assertLess(low, 47 / 89)
        self.assertGreater(high, 47 / 89)

    def test_impossible_counts_are_refused(self) -> None:
        for passes, trials in ((1, 0), (-1, 5), (6, 5)):
            with self.assertRaises(ValueError):
                wilson_interval(passes, trials)


class McNemarTests(unittest.TestCase):
    def test_no_discordant_tasks_show_no_difference(self) -> None:
        self.assertEqual(exact_mcnemar(0, 0), 1.0)

    def test_equal_discordant_counts_cap_at_one(self) -> None:
        self.assertEqual(exact_mcnemar(5, 5), 1.0)

    def test_one_against_nine_matches_the_binomial_tail(self) -> None:
        # 2 * (C(10,0) + C(10,1)) / 2**10
        self.assertAlmostEqual(exact_mcnemar(1, 9), 22 / 1024)
        self.assertEqual(exact_mcnemar(1, 9), exact_mcnemar(9, 1))


class ArmSpecificationTests(unittest.TestCase):
    def test_label_source_and_harness_split_at_the_right_separators(self) -> None:
        self.assertEqual(parse_arm("no harness=out/jobs/run one:bare"), ("no harness", Path("out/jobs/run one"), "bare"))

    def test_incomplete_specifications_are_refused(self) -> None:
        for spec in ("bare", "=path:bare", "label=path", "label=:bare", "label=path:"):
            with self.assertRaises(ValueError):
                parse_arm(spec)


PRICES = (0.45, 1.2, 0.10)


def outcome(task: str, grade: float | None, seconds: float, tokens: tuple[int, int, int] | None) -> Outcome:
    input_tokens, cache_tokens, output_tokens = tokens if tokens else (None, None, None)
    return Outcome(task, grade, None, input_tokens, cache_tokens, output_tokens, seconds)


class DollarsTests(unittest.TestCase):
    def test_cached_input_costs_less_than_new_input(self) -> None:
        # 400k new input, 600k cached, 100k output.
        self.assertAlmostEqual(dollars(outcome("a", 1.0, 1, (1_000_000, 600_000, 100_000)), PRICES), 0.36)

    def test_missing_counts_give_no_price(self) -> None:
        self.assertIsNone(dollars(outcome("a", 1.0, 1, None), PRICES))


class EfficiencyTests(unittest.TestCase):
    def test_ratio_is_the_geometric_mean_of_the_per_task_ratios(self) -> None:
        # The first arm takes half the time on one task and twice the time on the other.
        first = {"a": outcome("a", 1.0, 5, (10, 0, 10)), "b": outcome("b", 1.0, 20, (10, 0, 10))}
        second = {"a": outcome("a", 1.0, 10, (10, 0, 10)), "b": outcome("b", 1.0, 10, (10, 0, 10))}
        report = efficiency(first, second, PRICES)
        self.assertAlmostEqual(report["agent_seconds"]["ratio"], 1.0)
        self.assertAlmostEqual(report["output_tokens"]["ratio"], 1.0)

    def test_only_tasks_that_both_arms_passed_count(self) -> None:
        # The first arm looks fast on a task that it failed; that task must not count.
        first = {"a": outcome("a", 1.0, 10, (10, 0, 10)), "b": outcome("b", 0.0, 1, (10, 0, 10))}
        second = {"a": outcome("a", 1.0, 20, (10, 0, 10)), "b": outcome("b", 1.0, 20, (10, 0, 10))}
        report = efficiency(first, second, PRICES)
        self.assertEqual(report["matched_tasks"], 1)
        self.assertAlmostEqual(report["agent_seconds"]["ratio"], 0.5)

    def test_a_task_without_token_counts_still_counts_for_time(self) -> None:
        first = {"a": outcome("a", 1.0, 10, None), "b": outcome("b", 1.0, 10, (10, 0, 10))}
        second = {"a": outcome("a", 1.0, 20, (10, 0, 10)), "b": outcome("b", 1.0, 20, (10, 0, 10))}
        report = efficiency(first, second, PRICES)
        self.assertEqual(report["agent_seconds"]["tasks"], 2)
        self.assertEqual(report["output_tokens"]["tasks"], 1)

    def test_interval_surrounds_the_ratio_and_is_repeatable(self) -> None:
        first = {name: outcome(name, 1.0, seconds, (10, 0, 10)) for name, seconds in zip("abcdef", (4, 5, 6, 7, 8, 9))}
        second = {name: outcome(name, 1.0, 10, (10, 0, 10)) for name in "abcdef"}
        report = efficiency(first, second, PRICES)
        low, high = report["agent_seconds"]["interval"]
        self.assertLessEqual(low, report["agent_seconds"]["ratio"])
        self.assertGreaterEqual(high, report["agent_seconds"]["ratio"])
        self.assertEqual(report, efficiency(first, second, PRICES))


if __name__ == "__main__":
    unittest.main()
