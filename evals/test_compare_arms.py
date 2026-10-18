"""Checks of the interval, the paired test, and the arm specification."""

from pathlib import Path
import unittest

from evals.compare_arms import exact_mcnemar, parse_arm, wilson_interval, Z95


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


if __name__ == "__main__":
    unittest.main()
