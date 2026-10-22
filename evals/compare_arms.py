"""Compare harness arms on the same tasks, with one or more attempts at each task.

Each arm is LABEL=SOURCE:HARNESS. SOURCE is a Harbor job directory or a saved
outcomes file, and HARNESS is the agent name inside it. A source holds one
attempt at each of its tasks. A label can repeat. Sources with different tasks
add tasks, for example two benchmarks. Sources with the same tasks add attempts,
in the order given. Every arm needs the same tasks and the same number of
attempts at each task. A trial without a verifier reward counts as not passed
and is reported as ungraded, never as a graded failure. With
--drop-ungraded-tasks, a task that has an ungraded trial in any arm is removed
from every arm, so no arm gains or loses from a setup failure that it did not
cause.

  python -m evals.compare_arms --output summary.json --drop-ungraded-tasks \
      --arm "think=out/terminal-bench/jobs/JOB-1:think" \
      --arm "think=out/terminal-bench/jobs/JOB-2:think" \
      --arm "pi=out/terminal-bench/jobs/JOB-1:pi" \
      --arm "pi=out/terminal-bench/jobs/JOB-2:pi"
"""

import argparse
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from datetime import datetime
from fractions import Fraction
import json
import math
from pathlib import Path
import random
import statistics

RESAMPLES = 2000


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
    # What an audit found that makes the trial invalid. An invalid trial is not a pass.
    invalid: tuple[str, ...] = ()

    @property
    def passed(self) -> bool:
        return self.grade == 1.0 and not self.invalid


# One outcome for each attempt at a task, in the order that the attempts ran.
Attempts = tuple[Outcome, ...]
Arm = dict[str, Attempts]


def exact_sign_flip_p(differences: Sequence[int | Fraction]) -> float:
    """The two-sided exact p-value that paired differences sum to zero by chance.

    Without a real difference, each non-zero difference is as likely to be
    positive as negative, so every assignment of signs to the absolute
    differences is equally likely. With differences of +1 and -1 only, this is
    the exact McNemar test. Fractions are scaled to whole numbers first, so
    that the count of sign assignments stays exact."""
    exact = [Fraction(difference) for difference in differences]
    scale = math.lcm(*(value.denominator for value in exact)) if exact else 1
    whole = [int(value * scale) for value in exact]
    magnitudes = [abs(difference) for difference in whole if difference != 0]
    if not magnitudes:
        return 1.0
    ways = {0: 1}
    for magnitude in magnitudes:
        grown: dict[int, int] = {}
        for total, count in ways.items():
            for step in (magnitude, -magnitude):
                grown[total + step] = grown.get(total + step, 0) + count
        ways = grown
    observed = abs(sum(whole))
    return sum(count for total, count in ways.items() if abs(total) >= observed) / 2 ** len(magnitudes)


def pass_rate_interval(arm: Arm, *, resamples: int = RESAMPLES, seed: int = 1) -> tuple[float, float]:
    """The 95% interval of the pass rate, from resampling tasks. The attempts at
    one task move together, because they are not independent trials."""
    per_task = [sum(outcome.passed for outcome in attempts) / len(attempts) for attempts in arm.values()]
    generator = random.Random(seed)
    means = sorted(statistics.fmean(generator.choices(per_task, k=len(per_task))) for _ in range(resamples))
    return means[int(0.025 * resamples)], means[int(0.975 * resamples) - 1]


def _span_seconds(span: dict[str, str] | None) -> float | None:
    if not span or not span.get("started_at") or not span.get("finished_at"):
        return None
    return (datetime.fromisoformat(span["finished_at"]) - datetime.fromisoformat(span["started_at"])).total_seconds()


def trial_outcome(directory: Path, audit: Mapping[str, Mapping[str, object]]) -> tuple[str, Outcome]:
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
        invalid=tuple(str(flag) for flag in audit.get(f"{directory.parent.name}/{directory.name}", {}).get("invalid", ())),  # type: ignore[attr-defined]
    )


def load_arm(source: Path, harness: str, audit: Mapping[str, Mapping[str, object]] = {}) -> dict[str, Outcome]:
    """One attempt at each task of one source. The audit maps job/trial names to findings."""
    if source.is_dir():
        found = [trial_outcome(path.parent, audit) for path in sorted(source.glob("*/result.json"))]
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


def attempt_counts(arm: Arm) -> list[int]:
    """The distinct numbers of attempts that the tasks of an arm have."""
    return sorted({len(attempts) for attempts in arm.values()})


def summarize(arm: Arm, prices: tuple[float, float, float]) -> dict[str, object]:
    counts = attempt_counts(arm)
    outcomes = [outcome for task_attempts in arm.values() for outcome in task_attempts]
    passes = sum(outcome.passed for outcome in outcomes)
    low, high = pass_rate_interval(arm)
    metered = [(o, cost) for o in outcomes if (cost := dollars(o, prices)) is not None]
    metered_passes = sum(o.passed for o, _ in metered)
    total_cost = sum(cost for _, cost in metered)
    timed = [o for o in outcomes if o.agent_seconds is not None]
    timed_passes = sum(o.passed for o in timed)
    seconds = [o.agent_seconds for o in timed]
    setups = [o.setup_seconds for o in outcomes if o.setup_seconds is not None]
    return {
        "tasks": len(arm),
        "attempts_per_task": counts,
        "trials": len(outcomes),
        "passes": passes,
        # Only meaningful when every task has the same number of attempts.
        "passes_by_attempt": [sum(task_attempts[index].passed for task_attempts in arm.values()) for index in range(counts[0])] if len(counts) == 1 else None,
        # Each task counts once, whatever its number of attempts, as in the interval and the paired test.
        "pass_rate": statistics.fmean(sum(o.passed for o in task_attempts) / len(task_attempts) for task_attempts in arm.values()),
        "pass_rate_95_interval": [low, high],
        "ungraded": sum(o.grade is None for o in outcomes),
        "invalid": sum(bool(o.invalid) for o in outcomes),
        "exceptions": sum(o.exception is not None for o in outcomes),
        "metered_trials": len(metered),
        "dollars_per_metered_trial": total_cost / len(metered) if metered else None,
        # The price of one correct result: every metered trial counts, failures too.
        "dollars_per_pass": total_cost / metered_passes if metered_passes else None,
        "mean_input_tokens_including_cache": statistics.fmean(o.input_tokens for o, _ in metered if o.input_tokens is not None) if metered else None,
        "mean_output_tokens": statistics.fmean(o.output_tokens for o, _ in metered if o.output_tokens is not None) if metered else None,
        "median_agent_seconds": statistics.median(seconds) if seconds else None,
        "median_setup_seconds": statistics.median(setups) if setups else None,
        # The time to one correct result: every timed trial counts, failures too.
        "agent_seconds_per_pass": sum(seconds) / timed_passes if timed_passes else None,
    }


def compare(first: Arm, second: Arm) -> dict[str, object]:
    """Pair the arms by task. A task's difference is the pass rate of the first
    arm at that task minus that of the second, over the attempts at the task."""
    differences = {task: Fraction(sum(o.passed for o in first[task]) - sum(o.passed for o in second[task]), len(first[task])) for task in first}
    return {
        "first_better": sorted(task for task, difference in differences.items() if difference > 0),
        "second_better": sorted(task for task, difference in differences.items() if difference < 0),
        "tied": sum(difference == 0 for difference in differences.values()),
        "pass_rate_difference": float(sum(differences.values()) / len(differences)),
        "exact_sign_flip_p": exact_sign_flip_p(list(differences.values())),
    }


def _typical(attempts: Attempts, measure: Callable[[Outcome], float | None]) -> float | None:
    """The geometric mean of a measure over the passed attempts at one task, or
    none when no passed attempt has a usable value."""
    logs = [math.log(value) for outcome in attempts if outcome.passed and (value := measure(outcome)) is not None and value > 0]
    return math.exp(statistics.fmean(logs)) if logs else None


def efficiency(
    first: Arm,
    second: Arm,
    prices: tuple[float, float, float],
    *,
    resamples: int = RESAMPLES,
    seed: int = 1,
) -> dict[str, object]:
    """Compare the cost of the same work. Only passed attempts count, so an arm
    cannot look cheap by quitting early, and only tasks that both arms passed
    at least once. Each figure is the geometric mean over those tasks of the
    ratio of the first arm to the second, with a 95% interval from resampling
    tasks. A ratio below 1 means the first arm used less."""
    measures: dict[str, Callable[[Outcome], float | None]] = {
        "agent_seconds": lambda o: o.agent_seconds,
        "input_tokens_including_cache": lambda o: o.input_tokens,
        "output_tokens": lambda o: o.output_tokens,
        "dollars": lambda o: dollars(o, prices),
    }
    matched = sorted(task for task in first if any(o.passed for o in first[task]) and any(o.passed for o in second[task]))
    report: dict[str, object] = {"matched_tasks": len(matched)}
    for name, measure in measures.items():
        logs = []
        for task in matched:
            numerator, denominator = _typical(first[task], measure), _typical(second[task], measure)
            if numerator is not None and denominator is not None:
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


def pool_sources[Item](loaded: list[tuple[str, dict[str, Item]]]) -> dict[str, dict[str, tuple[Item, ...]]]:
    """Merge the sources that share a label. Sources with different tasks add
    tasks. A source with a task that an earlier source already holds adds an
    attempt at it."""
    gathered: dict[str, dict[str, list[Item]]] = {}
    for label, outcomes in loaded:
        tasks = gathered.setdefault(label, {})
        for task, outcome in outcomes.items():
            tasks.setdefault(task, []).append(outcome)
    return {label: {task: tuple(attempts) for task, attempts in tasks.items()} for label, tasks in gathered.items()}


def require_same_shape[Item](arms: dict[str, dict[str, tuple[Item, ...]]]) -> None:
    """Every arm needs the same tasks, and every task the same number of attempts in every arm."""
    shapes = {label: {task: len(attempts) for task, attempts in arm.items()} for label, arm in arms.items()}
    reference_label, reference = next(iter(shapes.items()))
    for label, shape in shapes.items():
        if shape.keys() != reference.keys():
            raise ValueError(f"Arms {reference_label!r} and {label!r} cover different tasks")
        if shape != reference:
            raise ValueError(f"Arms {reference_label!r} and {label!r} have different numbers of attempts")


def restrict_to_tasks[Item](arms: dict[str, dict[str, Item]], names: set[str]) -> dict[str, dict[str, Item]]:
    """Keep only the tasks that are listed, by their name after the last slash. A listed task that an arm lacks is an error."""
    restricted: dict[str, dict[str, Item]] = {}
    for label, arm in arms.items():
        by_name = {task.split("/")[-1]: task for task in arm}
        missing = sorted(names - by_name.keys())
        if missing:
            raise ValueError(f"Arm {label!r} lacks {len(missing)} of the listed tasks, for example {missing[0]!r}")
        restricted[label] = {by_name[name]: arm[by_name[name]] for name in sorted(names)}
    return restricted


def drop_ungraded_tasks(arms: dict[str, Arm]) -> tuple[dict[str, Arm], list[str]]:
    """Remove each task that has an ungraded trial in any arm, from every arm."""
    dropped = {task for arm in arms.values() for task, attempts in arm.items() if any(o.grade is None for o in attempts)}
    kept = {label: {task: attempts for task, attempts in arm.items() if task not in dropped} for label, arm in arms.items()}
    return kept, sorted(dropped)


def main() -> int:
    from evals.harbor_wire import MODEL, PROFILES

    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--arm", action="append", required=True, help="LABEL=SOURCE:HARNESS; the first label is the reference of the paired comparisons")
    parser.add_argument("--drop-ungraded-tasks", action="store_true", help="remove each task that has an ungraded trial in any arm, from every arm")
    parser.add_argument("--audit", type=Path, action="append", default=[], help="a file that evals.audit_trials wrote; a trial that it marks invalid counts as not passed")
    parser.add_argument("--tasks", type=Path, help="a file with one task name per line; every arm keeps only these tasks")
    parser.add_argument("--output", type=Path, required=True)
    arguments = parser.parse_args()
    specs = [parse_arm(spec) for spec in arguments.arm]
    if len(set(specs)) != len(specs):
        parser.error("A source is listed twice; list each attempt once")
    profile = PROFILES[MODEL]
    prices = (profile.price_input, profile.price_output, profile.price_cache_read)

    audit: dict[str, Mapping[str, object]] = {}
    for audit_file in arguments.audit:
        audit.update(json.loads(audit_file.read_text()))
    arms = pool_sources([(label, load_arm(source, harness, audit)) for label, source, harness in specs])
    if arguments.tasks:
        try:
            arms = restrict_to_tasks(arms, {line.strip().split("/")[-1] for line in arguments.tasks.read_text().splitlines() if line.strip()})
        except ValueError as error:
            parser.error(str(error))
    try:
        require_same_shape(arms)
    except ValueError as error:
        parser.error(str(error))
    dropped: list[str] = []
    if arguments.drop_ungraded_tasks:
        arms, dropped = drop_ungraded_tasks(arms)
        if not any(arms.values()):
            parser.error("Every task has an ungraded trial")
    labels = list(arms)
    report = {
        "model": MODEL,
        "prices_per_million_tokens": {"input": prices[0], "output": prices[1], "cache_read": prices[2]},
        "dropped_ungraded_tasks": dropped,
        "arms": {label: summarize(arm, prices) for label, arm in arms.items()},
        "paired_with_first": {label: compare(arms[labels[0]], arms[label]) for label in labels[1:]},
        "efficiency_of_first_against": {label: efficiency(arms[labels[0]], arms[label], prices) for label in labels[1:]},
    }
    arguments.output.write_text(json.dumps(report, indent=2) + "\n")

    if dropped:
        print(f"Dropped {len(dropped)} tasks with an ungraded trial in some arm: {', '.join(dropped)}")
    print(f"{'arm':<14}{'passed':>10}{'rate':>7}{'95% interval':>15}{'by attempt':>13}{'ungraded':>9}{'invalid':>8}{'errors':>7}{'$/trial':>9}{'$/pass':>8}{'out tok':>9}{'median s':>9}{'s/pass':>7}{'setup s':>8}")
    for label, row in report["arms"].items():
        assert isinstance(row, dict)
        low, high = row["pass_rate_95_interval"]

        def cell(value: object, width: int, digits: int) -> str:
            return " " * width if value is None else format(value, f"{width}.{digits}f")

        by_attempt = " ".join(str(count) for count in row["passes_by_attempt"]) if row["passes_by_attempt"] is not None else "mixed"
        print(
            f"{label:<14}{row['passes']:>5}/{row['trials']:<4}{100 * row['pass_rate']:>6.1f}%{100 * low:>9.1f}-{100 * high:<5.1f}{by_attempt:>13}"
            f"{row['ungraded']:>9}{row['invalid']:>8}{row['exceptions']:>7}"
            f"{cell(row['dollars_per_metered_trial'], 9, 3)}{cell(row['dollars_per_pass'], 8, 3)}"
            f"{cell(row['mean_output_tokens'], 9, 0)}{cell(row['median_agent_seconds'], 9, 0)}{cell(row['agent_seconds_per_pass'], 7, 0)}{cell(row['median_setup_seconds'], 8, 0)}"
        )

    print(f"\n{labels[0]} against each other arm, paired by task; a task counts for the arm that passed more attempts at it")
    print(f"{'against':<14}{'better':>8}{'worse':>7}{'tied':>6}{'difference':>12}{'exact p':>9}")
    for label, paired in report["paired_with_first"].items():
        assert isinstance(paired, dict)
        print(f"{label:<14}{len(paired['first_better']):>8}{len(paired['second_better']):>7}{paired['tied']:>6}{100 * paired['pass_rate_difference']:>+10.1f} pt{paired['exact_sign_flip_p']:>9.4f}")

    print(f"\n{labels[0]} over each other arm, as a ratio on the tasks that both passed; below 1 means {labels[0]} used less")
    print(f"{'against':<14}{'tasks':>6}  {'time':<20}{'input tokens':<20}{'output tokens':<20}{'dollars':<20}")
    for label, matched in report["efficiency_of_first_against"].items():
        assert isinstance(matched, dict)

        def ratio(measure: str) -> str:
            value = matched.get(measure)
            if not isinstance(value, dict):
                return "n/a"
            return f"{value['ratio']:.2f} ({value['interval'][0]:.2f}-{value['interval'][1]:.2f})"

        print(
            f"{label:<14}{matched['matched_tasks']:>6}  {ratio('agent_seconds'):<20}{ratio('input_tokens_including_cache'):<20}"
            f"{ratio('output_tokens'):<20}{ratio('dollars'):<20}"
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
