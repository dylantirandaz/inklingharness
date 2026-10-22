"""Checks of the paired test, the interval, the attempt handling, and the cost ratios."""

from fractions import Fraction
from pathlib import Path
import unittest

from evals.compare_arms import (
    Arm,
    Outcome,
    compare,
    dollars,
    drop_ungraded_tasks,
    efficiency,
    exact_sign_flip_p,
    parse_arm,
    pass_rate_interval,
    pool_sources,
    require_same_shape,
    restrict_to_tasks,
    summarize,
)

PRICES = (0.45, 1.2, 0.10)


def outcome(task: str, grade: float | None, seconds: float = 10, tokens: tuple[int, int, int] | None = (10, 0, 10)) -> Outcome:
    input_tokens, cache_tokens, output_tokens = tokens if tokens else (None, None, None)
    return Outcome(task, grade, None, input_tokens, cache_tokens, output_tokens, seconds)


def arm(**tasks: tuple[Outcome, ...]) -> Arm:
    return dict(tasks)


class SignFlipTests(unittest.TestCase):
    def test_no_difference_shows_nothing(self) -> None:
        self.assertEqual(exact_sign_flip_p([]), 1.0)
        self.assertEqual(exact_sign_flip_p([0, 0, 0]), 1.0)

    def test_equal_counts_of_wins_and_losses_cap_at_one(self) -> None:
        self.assertEqual(exact_sign_flip_p([1] * 5 + [-1] * 5), 1.0)

    def test_one_win_against_nine_losses_matches_the_binomial_tail(self) -> None:
        # With single attempts this is the exact McNemar test: 2 * (C(10,0) + C(10,1)) / 2**10.
        self.assertAlmostEqual(exact_sign_flip_p([1] + [-1] * 9), 22 / 1024)
        self.assertEqual(exact_sign_flip_p([1] + [-1] * 9), exact_sign_flip_p([-1] + [1] * 9))

    def test_a_larger_difference_on_one_task_weighs_more(self) -> None:
        # Of the 8 sign patterns of (2, 1, 1), only all-positive and all-negative reach a sum of 4.
        self.assertAlmostEqual(exact_sign_flip_p([2, 1, 1]), 2 / 8)
        # Losing two attempts on one task and winning one on another nets one attempt, which any pattern of signs can give.
        self.assertAlmostEqual(exact_sign_flip_p([-2, 1]), 1.0)

    def test_fractions_are_scaled_to_whole_numbers_so_the_count_stays_exact(self) -> None:
        # 1/2 and 1/3 become 3 and 2. Of the 4 sign patterns, (+,+) = 5 and (-,-) = -5 reach a sum of 5.
        self.assertAlmostEqual(exact_sign_flip_p([Fraction(1, 2), Fraction(1, 3)]), 2 / 4)
        # Whole numbers and fractions that mean the same give the same answer.
        self.assertEqual(exact_sign_flip_p([Fraction(2, 2), Fraction(-1, 1)]), exact_sign_flip_p([1, -1]))


class PassRateIntervalTests(unittest.TestCase):
    def test_a_second_attempt_that_repeats_the_first_adds_no_certainty(self) -> None:
        results = [1.0, 1.0, 1.0, 0.0, 0.0, 1.0, 0.0, 1.0, 1.0, 0.0]
        once = {f"t{i}": (outcome(f"t{i}", grade),) for i, grade in enumerate(results)}
        twice = {task: attempts + attempts for task, attempts in once.items()}
        self.assertEqual(pass_rate_interval(once), pass_rate_interval(twice))

    def test_interval_surrounds_the_rate_and_is_repeatable(self) -> None:
        results = [1.0, 1.0, 1.0, 0.0, 0.0, 1.0, 0.0, 1.0, 1.0, 0.0]
        sample = {f"t{i}": (outcome(f"t{i}", grade),) for i, grade in enumerate(results)}
        low, high = pass_rate_interval(sample)
        self.assertLess(low, 0.6)
        self.assertGreater(high, 0.6)
        self.assertEqual((low, high), pass_rate_interval(sample))

    def test_an_arm_that_always_passes_has_no_spread(self) -> None:
        sample = {f"t{i}": (outcome(f"t{i}", 1.0), outcome(f"t{i}", 1.0)) for i in range(5)}
        self.assertEqual(pass_rate_interval(sample), (1.0, 1.0))


class ArmSpecificationTests(unittest.TestCase):
    def test_label_source_and_harness_split_at_the_right_separators(self) -> None:
        self.assertEqual(parse_arm("no harness=out/jobs/run one:bare"), ("no harness", Path("out/jobs/run one"), "bare"))

    def test_incomplete_specifications_are_refused(self) -> None:
        for spec in ("bare", "=path:bare", "label=path", "label=:bare", "label=path:"):
            with self.assertRaises(ValueError):
                parse_arm(spec)


class PoolSourcesTests(unittest.TestCase):
    def test_sources_with_different_tasks_add_tasks(self) -> None:
        first = {"a": outcome("a", 1.0)}
        second = {"b": outcome("b", 0.0)}
        pooled = pool_sources([("think", first), ("pi", first), ("think", second)])
        self.assertEqual(sorted(pooled["think"]), ["a", "b"])
        self.assertEqual(sorted(pooled["pi"]), ["a"])

    def test_sources_with_the_same_task_add_attempts_in_the_order_given(self) -> None:
        first = {"a": outcome("a", 1.0), "b": outcome("b", 0.0)}
        second = {"a": outcome("a", 0.0), "b": outcome("b", 1.0)}
        pooled = pool_sources([("think", first), ("think", second)])
        self.assertEqual([o.passed for o in pooled["think"]["a"]], [True, False])
        self.assertEqual([o.passed for o in pooled["think"]["b"]], [False, True])


class SameShapeTests(unittest.TestCase):
    def test_arms_with_different_tasks_are_refused(self) -> None:
        with self.assertRaisesRegex(ValueError, "different tasks"):
            require_same_shape({"x": arm(a=(outcome("a", 1.0),)), "y": arm(b=(outcome("b", 1.0),))})

    def test_arms_with_different_numbers_of_attempts_are_refused(self) -> None:
        one, two = (outcome("a", 1.0),), (outcome("a", 1.0), outcome("a", 0.0))
        with self.assertRaisesRegex(ValueError, "numbers of attempts"):
            require_same_shape({"x": arm(a=one), "y": arm(a=two)})

    def test_tasks_may_differ_in_their_number_of_attempts_when_every_arm_matches(self) -> None:
        mixed = arm(a=(outcome("a", 1.0),), b=(outcome("b", 1.0), outcome("b", 0.0), outcome("b", 1.0)))
        require_same_shape({"x": mixed, "y": mixed})


class DropUngradedTests(unittest.TestCase):
    def test_a_task_with_an_ungraded_trial_in_any_arm_leaves_every_arm(self) -> None:
        arms = {
            "think": arm(a=(outcome("a", 1.0),), b=(outcome("b", 1.0),)),
            "bare": arm(a=(outcome("a", 0.0),), b=(outcome("b", None),)),
        }
        kept, dropped = drop_ungraded_tasks(arms)
        self.assertEqual(dropped, ["b"])
        self.assertEqual(sorted(kept["think"]), ["a"])
        self.assertEqual(sorted(kept["bare"]), ["a"])


class SummaryTests(unittest.TestCase):
    def test_passes_are_counted_by_attempt_and_priced_over_all_trials(self) -> None:
        sample = arm(
            a=(outcome("a", 1.0, 10, (1_000_000, 0, 0)), outcome("a", 0.0, 20, (1_000_000, 0, 0))),
            b=(outcome("b", 1.0, 30, (1_000_000, 0, 0)), outcome("b", 1.0, 40, (1_000_000, 0, 0))),
        )
        report = summarize(sample, PRICES)
        self.assertEqual(report["trials"], 4)
        self.assertEqual(report["passes"], 3)
        self.assertEqual(report["passes_by_attempt"], [2, 1])
        self.assertAlmostEqual(report["pass_rate"], 0.75)
        # Four trials at $0.45 each, three of them passed.
        self.assertAlmostEqual(report["dollars_per_pass"], 4 * 0.45 / 3)
        self.assertAlmostEqual(report["agent_seconds_per_pass"], 100 / 3)

    def test_tasks_with_different_numbers_of_attempts_count_once_each(self) -> None:
        # Task a has one attempt and passed; task b has three attempts and passed one of them.
        sample = arm(a=(outcome("a", 1.0),), b=(outcome("b", 1.0), outcome("b", 0.0), outcome("b", 0.0)))
        report = summarize(sample, PRICES)
        self.assertEqual(report["attempts_per_task"], [1, 3])
        self.assertEqual(report["trials"], 4)
        self.assertAlmostEqual(report["pass_rate"], (1 + 1 / 3) / 2)
        # Counting by attempt index has no meaning when the tasks have different numbers of attempts.
        self.assertIsNone(report["passes_by_attempt"])


class RestrictToTasksTests(unittest.TestCase):
    def test_tasks_are_matched_by_the_name_after_the_last_slash_and_the_rest_are_dropped(self) -> None:
        sample = {"swe-bench/a": (outcome("swe-bench/a", 1.0),), "swe-bench/b": (outcome("swe-bench/b", 0.0),), "swe-bench/c": (outcome("swe-bench/c", 1.0),)}
        kept = restrict_to_tasks({"x": sample, "y": sample}, {"a", "c"})
        self.assertEqual(sorted(kept["x"]), ["swe-bench/a", "swe-bench/c"])
        self.assertEqual(sorted(kept["y"]), ["swe-bench/a", "swe-bench/c"])

    def test_a_listed_task_that_an_arm_lacks_is_an_error_not_a_silent_shrink(self) -> None:
        full = {"a": (outcome("a", 1.0),), "b": (outcome("b", 1.0),)}
        with self.assertRaisesRegex(ValueError, "lacks 1 of the listed tasks"):
            restrict_to_tasks({"x": full, "y": {"a": full["a"]}}, {"a", "b"})


class InvalidTrialTests(unittest.TestCase):
    cheated = Outcome("a", 1.0, None, 10, 0, 10, 1.0, invalid=("used_model_api",))

    def test_a_trial_that_passed_but_is_invalid_is_not_a_pass(self) -> None:
        self.assertFalse(self.cheated.passed)
        self.assertTrue(outcome("a", 1.0).passed)

    def test_the_summary_counts_invalid_trials_and_does_not_count_them_as_passes(self) -> None:
        report = summarize(arm(a=(self.cheated,), b=(outcome("b", 1.0),)), PRICES)
        self.assertEqual((report["passes"], report["invalid"]), (1, 1))

    def test_the_pairing_and_the_cost_ratios_ignore_the_gains_of_an_invalid_trial(self) -> None:
        honest = arm(a=(outcome("a", 1.0, 10),))
        cheater = arm(a=(self.cheated,))
        self.assertEqual(compare(honest, cheater)["first_better"], ["a"])
        # No task is passed by both, so there is nothing to compare the cost of.
        self.assertEqual(efficiency(honest, cheater, PRICES)["matched_tasks"], 0)


class CompareTests(unittest.TestCase):
    def test_a_task_goes_to_the_arm_that_passed_more_attempts_at_it(self) -> None:
        win, loss = outcome("x", 1.0), outcome("x", 0.0)
        first = arm(a=(win, win), b=(win, loss), c=(loss, loss), d=(win, loss))
        second = arm(a=(loss, loss), b=(win, loss), c=(win, loss), d=(loss, win))
        report = compare(first, second)
        self.assertEqual(report["first_better"], ["a"])
        self.assertEqual(report["second_better"], ["c"])
        self.assertEqual(report["tied"], 2)
        self.assertAlmostEqual(report["pass_rate_difference"], 0.125)

    def test_single_attempts_give_the_mcnemar_result(self) -> None:
        win, loss = outcome("x", 1.0), outcome("x", 0.0)
        first = {f"t{i}": (win,) for i in range(9)} | {"s": (loss,)}
        second = {f"t{i}": (loss,) for i in range(9)} | {"s": (win,)}
        self.assertAlmostEqual(compare(first, second)["exact_sign_flip_p"], 22 / 1024)

    def test_tasks_with_different_numbers_of_attempts_count_once_each_in_the_pairing(self) -> None:
        win, loss = outcome("x", 1.0), outcome("x", 0.0)
        # Task a, two attempts: the first arm passed both, the second neither. Task b, three attempts: the reverse.
        first = arm(a=(win, win), b=(loss, loss, loss))
        second = arm(a=(loss, loss), b=(win, win, win))
        report = compare(first, second)
        self.assertEqual((report["first_better"], report["second_better"]), (["a"], ["b"]))
        self.assertAlmostEqual(report["pass_rate_difference"], 0.0)
        self.assertEqual(report["exact_sign_flip_p"], 1.0)


class DollarsTests(unittest.TestCase):
    def test_cached_input_costs_less_than_new_input(self) -> None:
        # 400k new input, 600k cached, 100k output.
        self.assertAlmostEqual(dollars(outcome("a", 1.0, 1, (1_000_000, 600_000, 100_000)), PRICES), 0.36)

    def test_missing_counts_give_no_price(self) -> None:
        self.assertIsNone(dollars(outcome("a", 1.0, 1, None), PRICES))


class EfficiencyTests(unittest.TestCase):
    def test_ratio_is_the_geometric_mean_of_the_per_task_ratios(self) -> None:
        # The first arm takes half the time on one task and twice the time on the other.
        first = arm(a=(outcome("a", 1.0, 5),), b=(outcome("b", 1.0, 20),))
        second = arm(a=(outcome("a", 1.0, 10),), b=(outcome("b", 1.0, 10),))
        report = efficiency(first, second, PRICES)
        self.assertAlmostEqual(report["agent_seconds"]["ratio"], 1.0)
        self.assertAlmostEqual(report["output_tokens"]["ratio"], 1.0)

    def test_only_tasks_that_both_arms_passed_count(self) -> None:
        # The first arm looks fast on a task that it failed; that task must not count.
        first = arm(a=(outcome("a", 1.0, 10),), b=(outcome("b", 0.0, 1),))
        second = arm(a=(outcome("a", 1.0, 20),), b=(outcome("b", 1.0, 20),))
        report = efficiency(first, second, PRICES)
        self.assertEqual(report["matched_tasks"], 1)
        self.assertAlmostEqual(report["agent_seconds"]["ratio"], 0.5)

    def test_a_failed_attempt_does_not_count_even_when_the_task_has_a_pass(self) -> None:
        # The first arm failed one attempt very quickly; only its 10 s pass counts.
        first = arm(a=(outcome("a", 0.0, 1), outcome("a", 1.0, 10)))
        second = arm(a=(outcome("a", 1.0, 20), outcome("a", 0.0, 400)))
        self.assertAlmostEqual(efficiency(first, second, PRICES)["agent_seconds"]["ratio"], 0.5)

    def test_several_passed_attempts_average_by_geometric_mean(self) -> None:
        first = arm(a=(outcome("a", 1.0, 4), outcome("a", 1.0, 16)))
        second = arm(a=(outcome("a", 1.0, 8),))
        # sqrt(4 * 16) = 8 seconds against 8 seconds.
        self.assertAlmostEqual(efficiency(first, second, PRICES)["agent_seconds"]["ratio"], 1.0)

    def test_a_task_without_token_counts_still_counts_for_time(self) -> None:
        first = arm(a=(outcome("a", 1.0, 10, None),), b=(outcome("b", 1.0, 10),))
        second = arm(a=(outcome("a", 1.0, 20),), b=(outcome("b", 1.0, 20),))
        report = efficiency(first, second, PRICES)
        self.assertEqual(report["agent_seconds"]["tasks"], 2)
        self.assertEqual(report["output_tokens"]["tasks"], 1)

    def test_interval_surrounds_the_ratio_and_is_repeatable(self) -> None:
        first = {name: (outcome(name, 1.0, seconds),) for name, seconds in zip("abcdef", (4, 5, 6, 7, 8, 9))}
        second = {name: (outcome(name, 1.0, 10),) for name in "abcdef"}
        report = efficiency(first, second, PRICES)
        low, high = report["agent_seconds"]["interval"]
        self.assertLessEqual(low, report["agent_seconds"]["ratio"])
        self.assertGreaterEqual(high, report["agent_seconds"]["ratio"])
        self.assertEqual(report, efficiency(first, second, PRICES))


if __name__ == "__main__":
    unittest.main()
