"""How many tasks a paired comparison of two harnesses needs.

Both harnesses run the same tasks. A task is discordant when one harness passes it and the other
fails it. Tasks that both pass or both fail say nothing about the difference. The exact paired test
(the sign test on the discordant tasks) therefore has a power that depends on two numbers:

  discordant  the share of tasks that are discordant (0.12 means 12 tasks of 100)
  difference  the gap between the success rates that the test must be able to find (0.05 means 5 points)

In the airgapped runs of this repository the discordant share was 0.11 to 0.18, so 0.12 is a fair
value for two close harnesses. A harness cannot show a gap larger than the discordant share.

  python -m evals.power_analysis                    # the table for discordant shares 0.12 and 0.18
  python -m evals.power_analysis --discordant 0.15  # the table for one share
"""

import argparse
from collections.abc import Sequence
from dataclasses import dataclass
from math import ceil, comb, sqrt
from statistics import NormalDist

DIFFERENCES = (0.02, 0.03, 0.05, 0.08, 0.10)


@dataclass(frozen=True)
class Design:
    """A paired comparison. Partial: difference must be above 0 and not above discordant, and discordant must be at most 1."""

    discordant: float
    difference: float
    alpha: float = 0.05
    power: float = 0.8

    def __post_init__(self) -> None:
        if not 0 < self.difference <= self.discordant <= 1:
            raise ValueError(f"need 0 < difference <= discordant <= 1, got difference={self.difference} and discordant={self.discordant}")
        if not (0 < self.alpha < 1 and 0 < self.power < 1):
            raise ValueError("alpha and power must be between 0 and 1")


def tasks_needed(design: Design) -> int:
    """The number of tasks for the power of the design, by the normal approximation of the McNemar test."""
    quantile = NormalDist().inv_cdf
    z_alpha, z_power = quantile(1 - design.alpha / 2), quantile(design.power)
    numerator = (z_alpha * sqrt(design.discordant) + z_power * sqrt(design.discordant - design.difference**2)) ** 2
    return ceil(numerator / design.difference**2)


def _binomial_p_value(wins: int, total: int) -> float:
    """The two-sided exact p value of wins out of total discordant tasks, when each outcome has probability 1/2."""
    if total == 0:
        return 1.0
    tail = sum(comb(total, k) for k in range(0, min(wins, total - wins) + 1))
    return min(1.0, 2 * tail / 2**total)


def exact_power(tasks: int, design: Design) -> float:
    """The power of the exact paired test with the given number of tasks, by summing over the number of discordant tasks.

    A discordant task goes to the first harness with probability (discordant + difference) / (2 * discordant)."""
    win = (design.discordant + design.difference) / (2 * design.discordant)
    power = 0.0
    for discordant_tasks in range(tasks + 1):
        weight = comb(tasks, discordant_tasks) * design.discordant**discordant_tasks * (1 - design.discordant) ** (tasks - discordant_tasks)
        if weight < 1e-12:
            continue
        rejecting = sum(
            comb(discordant_tasks, wins) * win**wins * (1 - win) ** (discordant_tasks - wins)
            for wins in range(discordant_tasks + 1)
            if _binomial_p_value(wins, discordant_tasks) <= design.alpha
        )
        power += weight * rejecting
    return power


def table(discordant: float, alpha: float, power: float) -> list[tuple[float, int]]:
    return [(difference, tasks_needed(Design(discordant, difference, alpha, power))) for difference in DIFFERENCES if difference <= discordant]


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--discordant", type=float, action="append", help="the share of discordant tasks; repeat for several tables (default 0.12 and 0.18)")
    parser.add_argument("--alpha", type=float, default=0.05)
    parser.add_argument("--power", type=float, default=0.8)
    arguments = parser.parse_args(argv)
    for discordant in arguments.discordant or [0.12, 0.18]:
        print(f"discordant share {discordant:.2f}, alpha {arguments.alpha}, power {arguments.power}: difference in points -> tasks")
        for difference, tasks in table(discordant, arguments.alpha, arguments.power):
            print(f"  {difference * 100:>4.0f} points  {tasks:>6} tasks")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
