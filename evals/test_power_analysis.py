"""Checks the number of tasks that a paired comparison needs."""

import unittest

from evals.power_analysis import Design, exact_power, table, tasks_needed


class TasksNeededTests(unittest.TestCase):
    def test_a_smaller_difference_needs_more_tasks(self) -> None:
        needed = [tasks_needed(Design(0.12, difference)) for difference in (0.10, 0.05, 0.03, 0.02)]
        self.assertEqual(needed, sorted(needed))
        self.assertEqual(len(set(needed)), 4)

    def test_more_discordant_tasks_need_more_tasks_for_the_same_difference(self) -> None:
        self.assertLess(tasks_needed(Design(0.12, 0.05)), tasks_needed(Design(0.18, 0.05)))

    def test_five_points_at_twelve_percent_discordance_need_about_three_hundred_seventy_tasks(self) -> None:
        self.assertAlmostEqual(tasks_needed(Design(0.12, 0.05)), 372, delta=6)

    def test_the_approximation_agrees_with_the_exact_test(self) -> None:
        design = Design(0.12, 0.05)
        needed = tasks_needed(design)
        # The exact test is a little less powerful than its normal approximation, so the power sits just under the goal.
        self.assertGreater(exact_power(needed, design), 0.72)
        self.assertLess(exact_power(needed // 2, design), 0.6)


class InvalidDesignTests(unittest.TestCase):
    def test_a_difference_above_the_discordant_share_cannot_exist_and_is_refused(self) -> None:
        with self.assertRaises(ValueError):
            Design(discordant=0.05, difference=0.08)

    def test_no_difference_is_refused(self) -> None:
        with self.assertRaises(ValueError):
            Design(discordant=0.12, difference=0.0)


class TableTests(unittest.TestCase):
    def test_the_table_leaves_out_differences_that_the_discordant_share_cannot_hold(self) -> None:
        self.assertEqual([difference for difference, _ in table(0.04, 0.05, 0.8)], [0.02, 0.03])


if __name__ == "__main__":
    unittest.main()
