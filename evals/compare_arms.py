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
from datetime import datetime
import json
import math
from pathlib import Path
import random
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
    # Time to install the harness in the sandbox before the agent starts.
    setup_seconds: float | None = None

    @property
    def passed(self) -> bool:
        return self.grade == 1.0


def _span_seconds(span: dict[str, str] | None) -> float | None:
    if not span or not span.get("started_at") or not span.get("finished_at"):
        return None
    return (datetime.fromisoformat(span["finished_at"]) - datetime.fromisoformat(span["started_at"])).total_seconds()


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
    else:
        seconds = _span_seconds(result.get("agent_execution"))
    # With a recorder, token counts are valid only when it saw every response
    # whole. Without one, the counts of the harness are valid when all three exist.
    recorded = metadata.get("all_recorded_usage_complete")
    if recorded is None:
        complete = all(agent.get(field) is not None for field in ("n_input_tokens", "n_cache_tokens", "n_output_tokens"))
    else:
        complete = recorded is True
    return result["agent_info"]["name"], Outcome(
        task=result["task_name"],
        grade=rewards.get("reward"),
        exception=exception["exception_type"] if exception else None,
        input_tokens=agent.get("n_input_tokens") if complete else None,
        cache_tokens=agent.get("n_cache_tokens") if complete else None,
        output_tokens=agent.get("n_output_tokens") if complete else None,
        agent_seconds=seconds,
        setup_seconds=_span_seconds(result.get("agent_setup")),
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


def dollars(outcome: Outcome, prices: tuple[float, float, float]) -> float | None:
    """prices are dollars per million uncached input, output, and cached input tokens."""
    if outcome.input_tokens is None or outcome.cache_tokens is None or outcome.output_tokens is None:
        return None
    price_input, price_output, price_cache = prices
    uncached = outcome.input_tokens - outcome.cache_tokens
    return (uncached * price_input + outcome.cache_tokens * price_cache + outcome.output_tokens * price_output) / 1e6


def summarize(arm: dict[str, Outcome], prices: tuple[float, float, float]) -> dict[str, object]:
    outcomes = list(arm.values())
    passes = sum(outcome.passed for outcome in outcomes)
    low, high = wilson_interval(passes, len(outcomes))
    metered = [(o, cost) for o in outcomes if (cost := dollars(o, prices)) is not None]
    metered_passes = sum(o.passed for o, _ in metered)
    cost = sum(cost for _, cost in metered)
    timed = [o for o in outcomes if o.agent_seconds is not None]
    timed_passes = sum(o.passed for o in timed)
    seconds = [o.agent_seconds for o in timed]
    return {
        "tasks": len(outcomes),
        "passes": passes,
        "pass_rate": passes / len(outcomes),
        "pass_rate_95_interval": [low, high],
        "ungraded": sum(o.grade is None for o in outcomes),
        "exceptions": sum(o.exception is not None for o in outcomes),
        "metered_tasks": len(metered),
        "dollars_per_metered_task": cost / len(metered) if metered else None,
        # The price of one correct result: every metered trial counts, failures too.
        "dollars_per_pass": cost / metered_passes if metered_passes else None,
        "mean_input_tokens_including_cache": statistics.fmean(o.input_tokens for o, _ in metered if o.input_tokens is not None) if metered else None,
        "mean_output_tokens": statistics.fmean(o.output_tokens for o, _ in metered if o.output_tokens is not None) if metered else None,
        "median_agent_seconds": statistics.median(seconds) if seconds else None,
        "median_setup_seconds": statistics.median(setups) if (setups := [o.setup_seconds for o in outcomes if o.setup_seconds is not None]) else None,
        # The time to one correct result: every timed trial counts, failures too.
        "agent_seconds_per_pass": sum(seconds) / timed_passes if timed_passes else None,
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


def efficiency(
    first: dict[str, Outcome],
    second: dict[str, Outcome],
    prices: tuple[float, float, float],
    *,
    resamples: int = 2000,
    seed: int = 1,
) -> dict[str, object]:
    """Compare the cost of the same work. Only the tasks that both arms passed
    count, so an arm cannot look cheap by quitting early. Each figure is the
    geometric mean of the per-task ratio of the first arm over the second, with
    a 95% interval from resampling tasks. A ratio below 1 means the first arm
    used less."""
    common = sorted(task for task in first if first[task].passed and second[task].passed)
    measures = {
        "agent_seconds": lambda o: o.agent_seconds,
        "input_tokens_including_cache": lambda o: o.input_tokens,
        "output_tokens": lambda o: o.output_tokens,
        "dollars": lambda o: dollars(o, prices),
    }
    report: dict[str, object] = {"matched_tasks": len(common)}
    for name, measure in measures.items():
        logs = []
        for task in common:
            numerator, denominator = measure(first[task]), measure(second[task])
            if numerator is not None and denominator is not None and numerator > 0 and denominator > 0:
                logs.append(math.log(numerator / denominator))
        if not logs:
            report[name] = None
            continue
        generator = random.Random(seed)
        means = sorted(statistics.fmean(generator.choices(logs, k=len(logs))) for _ in range(resamples))
        report[name] = {
            "tasks": len(logs),
            "ratio": math.exp(statistics.fmean(logs)),
            "interval": [math.exp(means[int(0.025 * resamples)]), math.exp(means[int(0.975 * resamples) - 1])],
        }
    return report


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
        "efficiency_of_first_against": {label: efficiency(arms[labels[0]], arms[label], prices) for label in labels[1:]},
    }
    arguments.output.write_text(json.dumps(report, indent=2) + "\n")
    print(f"{'arm':<28}{'pass':>10}{'95% interval':>16}{'ungraded':>10}{'errors':>8}{'$/task':>9}{'$/pass':>8}{'out tok':>10}{'median s':>10}{'s/pass':>8}{'setup s':>8}")
    for label, row in report["arms"].items():
        assert isinstance(row, dict)
        low, high = row["pass_rate_95_interval"]

        def cell(value: object, width: int, digits: int) -> str:
            return " " * width if value is None else format(value, f"{width}.{digits}f")

        print(
            f"{label:<28}{row['passes']:>4}/{row['tasks']:<4}{100 * low:>8.1f}-{100 * high:<6.1f}"
            f"{row['ungraded']:>10}{row['exceptions']:>8}"
            f"{cell(row['dollars_per_metered_task'], 9, 3)}{cell(row['dollars_per_pass'], 8, 3)}"
            f"{cell(row['mean_output_tokens'], 10, 0)}{cell(row['median_agent_seconds'], 10, 0)}{cell(row['agent_seconds_per_pass'], 8, 0)}{cell(row['median_setup_seconds'], 8, 0)}"
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
