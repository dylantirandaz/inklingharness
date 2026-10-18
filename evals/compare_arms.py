"""Compare harness arms on the same Terminal-Bench tasks.

Each arm is LABEL=SOURCE:HARNESS. SOURCE is a Harbor job directory or a saved
outcomes file, and HARNESS is the agent name inside it. Every arm must hold one
outcome for each task. A trial without a verifier reward counts as not passed
and is reported as ungraded, never as a graded failure.

  python -m evals.compare_arms --output summary.json \
      --arm "no harness=out/terminal-bench/jobs/JOB:bare" \
      --arm "think=out/terminal-bench/jobs/JOB:inkling" \
      --arm "pi=out/terminal-bench/native-fixed-baseline-445-20261010.json:pi"
"""

import argparse
from dataclasses import dataclass
import json
import math
from pathlib import Path
import statistics

Z95 = 1.959964


def wilson_interval(passes: int, trials: int) -> tuple[float, float]:
    """The 95% Wilson score interval of a pass rate."""
    if trials <= 0 or not 0 <= passes <= trials:
        raise ValueError(f"Invalid counts: {passes} of {trials}")
    rate = passes / trials
    scale = 1 + Z95**2 / trials
    center = (rate + Z95**2 / (2 * trials)) / scale
    half = Z95 * math.sqrt(rate * (1 - rate) / trials + Z95**2 / (4 * trials**2)) / scale
    # The interval of an arm that never or always passes ends at 0 or 1 exactly.
    low = 0.0 if passes == 0 else max(0.0, center - half)
    high = 1.0 if passes == trials else min(1.0, center + half)
    return low, high


def exact_mcnemar(only_first: int, only_second: int) -> float:
    """The two-sided exact p-value that two arms pass equally often, from the
    tasks only one of them passed."""
    discordant = only_first + only_second
    if discordant == 0:
        return 1.0
    tail = sum(math.comb(discordant, count) for count in range(min(only_first, only_second) + 1))
    return min(1.0, 2 * tail / 2**discordant)


@dataclass(frozen=True)
class Outcome:
    task: str
    grade: float | None
    exception: str | None
    input_tokens: int | None
    cache_tokens: int | None
    output_tokens: int | None
    agent_seconds: float | None

    @property
    def passed(self) -> bool:
        return self.grade == 1.0


def _trial_outcome(directory: Path) -> tuple[str, Outcome]:
    result = json.loads((directory / "result.json").read_text())
    rewards = (result.get("verifier_result") or {}).get("rewards") or {}
    agent = result.get("agent_result") or {}
    metadata = agent.get("metadata") or {}
    exception = result.get("exception_info")
    seconds = None
    run = directory / "agent" / "wire" / "run.json"
    if run.exists():
        seconds = json.loads(run.read_text()).get("elapsed_seconds")
    # Token counts are valid only when the recorder saw every response whole.
    complete = metadata.get("all_recorded_usage_complete") is True
    return result["agent_info"]["name"], Outcome(
        task=result["task_name"],
        grade=rewards.get("reward"),
        exception=exception["exception_type"] if exception else None,
        input_tokens=agent.get("n_input_tokens") if complete else None,
        cache_tokens=agent.get("n_cache_tokens") if complete else None,
        output_tokens=agent.get("n_output_tokens") if complete else None,
        agent_seconds=seconds,
    )


def load_arm(source: Path, harness: str) -> dict[str, Outcome]:
    if source.is_dir():
        found = [_trial_outcome(path.parent) for path in sorted(source.glob("*/result.json"))]
        found = [(name, outcome) for name, outcome in found if name == harness]
    else:
        saved = json.loads(source.read_text())["outcomes"]
        found = [
            (
                entry["harness"],
                Outcome(
                    task=entry["task"],
                    grade=entry["grade"],
                    exception=entry["exception"],
                    input_tokens=entry["input_tokens"] if entry["complete_usage"] else None,
                    cache_tokens=entry["cache_tokens"] if entry["complete_usage"] else None,
                    output_tokens=entry["output_tokens"] if entry["complete_usage"] else None,
                    agent_seconds=entry["agent_elapsed_seconds"],
                ),
            )
            for entry in saved
        ]
        found = [(name, outcome) for name, outcome in found if name == harness]
    arm: dict[str, Outcome] = {}
    for _, outcome in found:
        if outcome.task in arm:
            raise ValueError(f"Two outcomes for {outcome.task} in {source} ({harness})")
        arm[outcome.task] = outcome
    if not arm:
        raise ValueError(f"No outcomes for {harness} in {source}")
    return arm


def summarize(arm: dict[str, Outcome], prices: tuple[float, float, float]) -> dict[str, object]:
    """prices are dollars per million uncached input, output, and cached input tokens."""
    price_input, price_output, price_cache = prices
    outcomes = list(arm.values())
    passes = sum(outcome.passed for outcome in outcomes)
    low, high = wilson_interval(passes, len(outcomes))
    metered = [o for o in outcomes if o.input_tokens is not None and o.cache_tokens is not None and o.output_tokens is not None]
    cost = sum(
        ((o.input_tokens - o.cache_tokens) * price_input + o.cache_tokens * price_cache + o.output_tokens * price_output) / 1e6
        for o in metered
        if o.input_tokens is not None and o.cache_tokens is not None and o.output_tokens is not None
    )
    seconds = [o.agent_seconds for o in outcomes if o.agent_seconds is not None]
    return {
        "tasks": len(outcomes),
        "passes": passes,
        "pass_rate": passes / len(outcomes),
        "pass_rate_95_interval": [low, high],
        "ungraded": sum(o.grade is None for o in outcomes),
        "exceptions": sum(o.exception is not None for o in outcomes),
        "metered_tasks": len(metered),
        "dollars_per_metered_task": cost / len(metered) if metered else None,
        "mean_input_tokens_including_cache": statistics.fmean(o.input_tokens for o in metered if o.input_tokens is not None) if metered else None,
        "mean_output_tokens": statistics.fmean(o.output_tokens for o in metered if o.output_tokens is not None) if metered else None,
        "median_agent_seconds": statistics.median(seconds) if seconds else None,
    }


def compare(first: dict[str, Outcome], second: dict[str, Outcome]) -> dict[str, object]:
    only_first = sorted(task for task in first if first[task].passed and not second[task].passed)
    only_second = sorted(task for task in first if second[task].passed and not first[task].passed)
    return {
        "both_passed": sum(first[task].passed and second[task].passed for task in first),
        "only_first_passed": only_first,
        "only_second_passed": only_second,
        "mcnemar_exact_p": exact_mcnemar(len(only_first), len(only_second)),
    }


def parse_arm(spec: str) -> tuple[str, Path, str]:
    label, separator, rest = spec.partition("=")
    source, colon, harness = rest.rpartition(":")
    if not separator or not colon or not label or not source or not harness:
        raise ValueError(f"Expected LABEL=SOURCE:HARNESS, got {spec!r}")
    return label, Path(source), harness


def main() -> int:
    from evals.harbor_wire import MODEL, PROFILES

    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--arm", action="append", required=True, help="LABEL=SOURCE:HARNESS; the first arm is the reference of the paired comparisons")
    parser.add_argument("--output", type=Path, required=True)
    arguments = parser.parse_args()
    profile = PROFILES[MODEL]
    prices = (profile.price_input, profile.price_output, profile.price_cache_read)

    arms = {label: load_arm(source, harness) for label, source, harness in map(parse_arm, arguments.arm)}
    if len(arms) != len(arguments.arm):
        parser.error("Arm labels must be unique")
    task_sets = {frozenset(arm) for arm in arms.values()}
    if len(task_sets) != 1:
        parser.error("Every arm must cover the same tasks")
    labels = list(arms)
    report = {
        "model": MODEL,
        "prices_per_million_tokens": {"input": prices[0], "output": prices[1], "cache_read": prices[2]},
        "arms": {label: summarize(arm, prices) for label, arm in arms.items()},
        "paired_with_first": {label: compare(arms[labels[0]], arms[label]) for label in labels[1:]},
    }
    arguments.output.write_text(json.dumps(report, indent=2) + "\n")
    print(f"{'arm':<28}{'pass':>10}{'95% interval':>16}{'ungraded':>10}{'errors':>8}{'$/task':>9}{'out tok':>10}{'median s':>10}")
    for label, row in report["arms"].items():
        assert isinstance(row, dict)
        low, high = row["pass_rate_95_interval"]
        dollars = row["dollars_per_metered_task"]
        output = row["mean_output_tokens"]
        print(
            f"{label:<28}{row['passes']:>4}/{row['tasks']:<4}{100 * low:>8.1f}-{100 * high:<6.1f}"
            f"{row['ungraded']:>10}{row['exceptions']:>8}"
            f"{'' if dollars is None else format(dollars, '9.3f')}"
            f"{'' if output is None else format(output, '10.0f')}"
            f"{row['median_agent_seconds']:>10.0f}"
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
