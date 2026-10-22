"""The measures of Arena's HarnessTax study, computed from Harbor job folders.

HarnessTax (Pan et al., Arena, September 2026) compares coding-agent harnesses that run
one model. This module computes each measure of the post, with its definitions:

  success, cost    For one harness: the success rate of an attempt, and its token cost
                   at a fixed list price, each averaged over the attempts at a task and
                   then over tasks. A 95% interval comes from resampling the tasks with
                   replacement, 10,000 times.
  Pareto frontier  The harnesses that no other harness beats on both axes: at most the
                   same cost and at least the same success rate. This module also gives
                   the share of resamples in which a harness is on the frontier.
  cost ratio       The mean cost of a harness over the mean cost of a reference.
  harness effect   The success rate of a harness minus the mean over all harnesses.
  turns            Model calls per attempt, and the cost of one call.
  cumulative curve Sort the attempts from the cheapest to the dearest, group equal
                   costs, and add them up. Both the cost and the successes are divided
                   by the number of attempts, and a failed attempt adds cost without
                   success. The last point is the mean cost and the success rate. The
                   post also draws the curve against turns and against tokens.
  first call       The input tokens that the provider reports for the first model call,
                   as a mean and a standard deviation.

  python -m evals.harness_tax --name SWE-bench --output swe.json --audit audit.json \\
      --arm think=JOB:think --arm think=OTHER_JOB:think --arm pi=JOB:pi ...
"""

import argparse
from bisect import bisect_right
from collections.abc import Callable, Iterator, Mapping, Sequence
from dataclasses import dataclass
import json
import math
from pathlib import Path
import random
import statistics
from typing import Literal

from evals.compare_arms import dollars, parse_arm, pool_sources, require_same_shape, restrict_to_tasks, trial_outcome

RESAMPLES = 10_000
# The post caps an attempt at 100 agent turns. These runs set no cap, so the report counts the attempts above it.
TURN_CAP = 100
Measure = Literal["dollars", "turns", "tokens"]
MEASURES: tuple[Measure, ...] = ("dollars", "turns", "tokens")
# Budgets of the cumulative curves, as multiples of the lowest mean spend among the harnesses.
BUDGET_MULTIPLES = (0.5, 0.75, 1.0, 1.25, 1.5)


@dataclass(frozen=True)
class Transcript:
    """What a harness wrote about the model calls of one attempt."""

    turns: int
    first_call_tokens: int | None


@dataclass(frozen=True)
class Rollout:
    """One attempt at one task. A measure that the trial does not report is None."""

    passed: bool
    dollars: float | None
    tokens: int | None
    turns: int | None
    first_call_tokens: int | None


Arm = dict[str, tuple[Rollout, ...]]


def _prompt_tokens(usage: Mapping[str, object]) -> int:
    """Input tokens in the Anthropic form, where cached tokens come apart from the others."""
    return sum(int(usage.get(key) or 0) for key in ("input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"))  # type: ignore[call-overload]


def _event_transcript(path: Path, kind_key: str) -> Transcript:
    turns = []
    for line in path.read_text(errors="replace").splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get(kind_key) == "turn":
            turns.append(event)
    usage = turns[0].get("usage") if turns else None
    return Transcript(turns=len(turns), first_call_tokens=_prompt_tokens(usage) if isinstance(usage, dict) else None)


def _atif_transcript(path: Path) -> Transcript:
    steps = json.loads(path.read_text()).get("steps", [])
    agent_steps = [step for step in steps if step.get("source") == "agent"]
    first = ((agent_steps[0].get("metrics") or {}).get("prompt_tokens")) if agent_steps else None
    return Transcript(turns=sum(int(step.get("llm_call_count") or 1) for step in agent_steps), first_call_tokens=int(first) if first is not None else None)


def read_transcript(harness: str, directory: Path) -> Transcript:
    """The model calls of a trial. Think and the baseline write their own event files. The other harnesses
    write an ATIF trajectory, where an agent step is one model response. Raises FileNotFoundError when
    the harness wrote no transcript."""
    match harness:
        case "think":
            return _event_transcript(directory / "agent" / "think-events.jsonl", "type")
        case "bare":
            return _event_transcript(directory / "agent" / "bare-events.jsonl", "event")
        case "claude-code" | "codex" | "pi":
            return _atif_transcript(directory / "agent" / "trajectory.json")
        case _:
            raise ValueError(f"No transcript reader for the harness {harness!r}")


def load_rollouts(source: Path, harness: str, audit: Mapping[str, Mapping[str, object]], prices: tuple[float, float, float]) -> dict[str, Rollout]:
    """One attempt at each task of one job folder. A trial that the audit marks invalid is not a pass."""
    rollouts: dict[str, Rollout] = {}
    for result_path in sorted(source.glob("*/result.json")):
        name, outcome = trial_outcome(result_path.parent, audit)
        if name != harness:
            continue
        if outcome.grade is None:
            raise ValueError(f"{result_path.parent} is ungraded; remove its task from every arm first")
        try:
            transcript: Transcript | None = read_transcript(harness, result_path.parent)
        except FileNotFoundError:
            transcript = None  # A timed-out trial can end before its harness writes a transcript.
        if outcome.task in rollouts:
            raise ValueError(f"Two trials at {outcome.task} in {source} ({harness})")
        tokens = None if outcome.input_tokens is None or outcome.output_tokens is None else outcome.input_tokens + outcome.output_tokens
        rollouts[outcome.task] = Rollout(
            passed=outcome.passed,
            dollars=dollars(outcome, prices),
            tokens=tokens,
            turns=transcript.turns if transcript else None,
            first_call_tokens=transcript.first_call_tokens if transcript else None,
        )
    if not rollouts:
        raise ValueError(f"No trials of {harness} in {source}")
    return rollouts


def spend(rollout: Rollout, measure: Measure) -> float | None:
    match measure:
        case "dollars":
            return rollout.dollars
        case "turns":
            return None if rollout.turns is None else float(rollout.turns)
        case "tokens":
            return None if rollout.tokens is None else float(rollout.tokens)


def task_means(arm: Arm, measure: Callable[[Rollout], float | None]) -> dict[str, float]:
    """The mean of a measure over the attempts at each task. A task where no attempt has the measure is left out."""
    means: dict[str, float] = {}
    for task, attempts in arm.items():
        values = [value for rollout in attempts if (value := measure(rollout)) is not None]
        if values:
            means[task] = sum(values) / len(values)
    return means


def _resampled_indices(count: int, resamples: int, seed: int) -> Iterator[list[int]]:
    """The same resamples for every harness keep their tasks paired."""
    generator = random.Random(seed)
    for _ in range(resamples):
        yield generator.choices(range(count), k=count)


def bootstrap_interval(values: Sequence[float], *, resamples: int = RESAMPLES, seed: int = 1) -> tuple[float, float]:
    """The 95% interval of the mean, from resampling the values with replacement."""
    means = sorted(sum(values[i] for i in indices) / len(values) for indices in _resampled_indices(len(values), resamples, seed))
    return means[int(0.025 * resamples)], means[int(0.975 * resamples) - 1]


def ratio_interval(numerator: Sequence[float], denominator: Sequence[float], *, resamples: int = RESAMPLES, seed: int = 1) -> tuple[float, float, float]:
    """The ratio of the two means and its 95% interval, from resampling the tasks of both together."""
    ratios = sorted(sum(numerator[i] for i in indices) / sum(denominator[i] for i in indices) for indices in _resampled_indices(len(numerator), resamples, seed))
    return sum(numerator) / sum(denominator), ratios[int(0.025 * resamples)], ratios[int(0.975 * resamples) - 1]


def pareto_frontier(points: Mapping[str, tuple[float, float]]) -> frozenset[str]:
    """points maps a harness to its (cost, success rate). A harness is on the frontier when no other
    harness has at most its cost and at least its success rate, with one of the two strictly better."""
    return frozenset(
        name
        for name, (cost, success) in points.items()
        if not any(
            other_cost <= cost and other_success >= success and (other_cost < cost or other_success > success)
            for other, (other_cost, other_success) in points.items()
            if other != name
        )
    )


def frontier_frequencies(costs: Mapping[str, Sequence[float]], successes: Mapping[str, Sequence[float]], *, resamples: int = RESAMPLES, seed: int = 1) -> dict[str, float]:
    """The share of task resamples in which each harness is on the Pareto frontier.
    costs[label][i] and successes[label][i] belong to the same task i, in the same order for every label."""
    count = len(next(iter(costs.values())))
    wins = dict.fromkeys(costs, 0)
    for indices in _resampled_indices(count, resamples, seed):
        points = {label: (sum(costs[label][i] for i in indices) / count, sum(successes[label][i] for i in indices) / count) for label in costs}
        for label in pareto_frontier(points):
            wins[label] += 1
    return {label: won / resamples for label, won in wins.items()}


def harness_effects(success: Mapping[str, float]) -> dict[str, float]:
    mean = statistics.fmean(success.values())
    return {label: rate - mean for label, rate in success.items()}


def tasks_to_resolve_success(per_task_differences: Sequence[float], half_width: float) -> int:
    """The tasks that a 95% interval of the mean success difference needs to be no wider than half_width
    on each side. It takes the spread of the observed per-task differences as the spread of the new tasks."""
    return math.ceil((1.96 * statistics.stdev(per_task_differences) / half_width) ** 2)


def tasks_to_resolve_ratio(tasks_now: int, interval: tuple[float, float, float], percent: float) -> int:
    """The tasks that the interval of a cost ratio needs to be no wider than percent on each side. The interval
    narrows with the square root of the tasks, so this scales the half-width of the interval on the log scale."""
    _, low, high = interval
    half_width_now = (math.log(high) - math.log(low)) / 2
    return math.ceil(tasks_now * (half_width_now / math.log(1 + percent / 100)) ** 2)


@dataclass(frozen=True)
class Curve:
    """y[k] is the share of all attempts that succeeded when the cheapest attempts, up to group k, are
    counted, and x[k] is the spend of those attempts divided by the number of all attempts."""

    x: tuple[float, ...]
    y: tuple[float, ...]

    def success_within(self, budget: float) -> float:
        """The curve at a spend per attempt, with straight lines between the points. It stays level past the end."""
        if budget <= 0:
            return 0.0
        if budget >= self.x[-1]:
            return self.y[-1]
        k = bisect_right(self.x, budget)
        weight = (budget - self.x[k - 1]) / (self.x[k] - self.x[k - 1])
        return self.y[k - 1] + weight * (self.y[k] - self.y[k - 1])


def cumulative_curve(attempts: Sequence[tuple[float, bool]]) -> Curve:
    """attempts holds the spend and the success of each completed attempt."""
    if not attempts:
        raise ValueError("A curve needs at least one attempt")
    ordered = sorted(attempts)
    count = len(ordered)
    xs, ys = [0.0], [0.0]
    total, won, start = 0.0, 0, 0
    while start < count:
        end = start
        while end < count and ordered[end][0] == ordered[start][0]:
            end += 1
        for amount, passed in ordered[start:end]:
            total += amount
            won += passed
        xs.append(total / count)
        ys.append(won / count)
        start = end
    return Curve(tuple(xs), tuple(ys))


def mean_and_deviation(values: Sequence[float]) -> tuple[float, float]:
    """The mean and the sample standard deviation, which is 0 for one value."""
    return statistics.fmean(values), statistics.stdev(values) if len(values) > 1 else 0.0


def analyze(arms: dict[str, Arm], *, reference: str, own: str | None = None, resamples: int = RESAMPLES) -> dict[str, object]:
    """Every measure of the post for arms that cover the same tasks. reference is the arm of the cost
    ratios. own is the harness of the model's provider, for the question of whether it is the best."""
    require_same_shape(arms)
    if reference not in arms or (own is not None and own not in arms):
        raise ValueError(f"The reference {reference!r} and the own harness {own!r} must be arms")
    labels = list(arms)
    tasks = sorted(arms[reference])
    success = {label: task_means(arms[label], lambda r: float(r.passed)) for label in labels}
    cost = {label: task_means(arms[label], lambda r: r.dollars) for label in labels}
    # A task enters the cost measures only when every harness reports a cost at it, so that the tasks stay paired.
    paired = [task for task in tasks if all(task in cost[label] for label in labels)]
    report: dict[str, object] = {"tasks": len(tasks), "tasks_with_cost_for_every_harness": len(paired), "attempts_per_task": sorted({len(a) for a in arms[reference].values()})}

    rows: dict[str, dict[str, object]] = {}
    for label in labels:
        all_attempts = [rollout for attempts in arms[label].values() for rollout in attempts]
        successes = [success[label][task] for task in tasks]
        costs = [cost[label][task] for task in paired]
        row: dict[str, object] = {
            "attempts": len(all_attempts),
            "success": statistics.fmean(successes),
            "success_interval": bootstrap_interval(successes, resamples=resamples),
            "dollars_per_attempt": statistics.fmean(costs),
            "dollars_interval": bootstrap_interval(costs, resamples=resamples),
        }
        for measure in ("turns", "tokens"):
            per_task = task_means(arms[label], lambda r, m=measure: spend(r, m))  # type: ignore[misc]
            values = [per_task[task] for task in tasks if task in per_task]
            row[f"{measure}_per_attempt"] = statistics.fmean(values) if values else None
        both = [(r.dollars, r.turns) for r in all_attempts if r.dollars is not None and r.turns]
        row["dollars_per_turn"] = sum(d for d, _ in both) / sum(t for _, t in both) if both else None
        turns = [r.turns for r in all_attempts if r.turns is not None]
        row["most_turns_in_an_attempt"] = max(turns) if turns else None
        row["attempts_over_turn_cap"] = sum(t > TURN_CAP for t in turns)
        first = [float(r.first_call_tokens) for r in all_attempts if r.first_call_tokens is not None]
        row["first_call_tokens"] = mean_and_deviation(first) if first else None
        row["attempts_without_usage"] = sum(r.dollars is None for r in all_attempts)
        rows[label] = row
    report["harnesses"] = rows

    points = {label: (statistics.fmean(cost[label][task] for task in paired), statistics.fmean(success[label][task] for task in paired)) for label in labels}
    report["pareto_frontier"] = sorted(pareto_frontier(points))
    report["frontier_share_of_resamples"] = frontier_frequencies(
        {label: [cost[label][task] for task in paired] for label in labels}, {label: [success[label][task] for task in paired] for label in labels}, resamples=resamples
    )
    report["cost_ratio_to_reference"] = {
        label: ratio_interval([cost[label][task] for task in paired], [cost[reference][task] for task in paired], resamples=resamples) for label in labels if label != reference
    }
    report["tasks_to_resolve_against_reference"] = {
        label: {
            "success_gap_of_2_points": tasks_to_resolve_success([success[reference][task] - success[label][task] for task in tasks], 0.02),
            "cost_gap_of_5_percent": tasks_to_resolve_ratio(len(paired), report["cost_ratio_to_reference"][label], 5.0),  # type: ignore[index]
        }
        for label in labels
        if label != reference
    }
    report["harness_effect_on_success"] = harness_effects({label: float(rows[label]["success"]) for label in labels})  # type: ignore[arg-type]
    best_success = max(float(rows[label]["success"]) for label in labels)  # type: ignore[arg-type]
    lowest_cost = min(float(rows[label]["dollars_per_attempt"]) for label in labels)  # type: ignore[arg-type]
    report["highest_success"] = [label for label in labels if float(rows[label]["success"]) == best_success]  # type: ignore[arg-type]
    report["lowest_cost"] = [label for label in labels if float(rows[label]["dollars_per_attempt"]) == lowest_cost]  # type: ignore[arg-type]
    if own is not None:
        report["own_harness"] = {"label": own, "has_highest_success": own in report["highest_success"], "has_lowest_cost": own in report["lowest_cost"]}  # type: ignore[operator]

    curves: dict[str, object] = {}
    for measure in MEASURES:
        by_label = {
            label: cumulative_curve([(amount, r.passed) for attempts in arms[label].values() for r in attempts if (amount := spend(r, measure)) is not None]) for label in labels
        }
        lowest = min(curve.x[-1] for curve in by_label.values())
        curves[measure] = {
            "budgets": [lowest * multiple for multiple in BUDGET_MULTIPLES],
            "mean_spend_and_success": {label: (curve.x[-1], curve.y[-1]) for label, curve in by_label.items()},
            "success_within_budget": {label: [curve.success_within(lowest * multiple) for multiple in BUDGET_MULTIPLES] for label, curve in by_label.items()},
        }
    report["cumulative_curves"] = curves
    return report


def _percent(value: float) -> str:
    return f"{100 * value:5.1f}%"


def format_report(name: str, report: dict[str, object], prices: tuple[float, float, float], request_context: Mapping[str, Mapping[str, object]] = {}) -> str:
    rows: dict[str, dict[str, object]] = report["harnesses"]  # type: ignore[assignment]
    lines = [f"== {name}: {report['tasks']} tasks, {report['attempts_per_task']} attempts per task; prices ${prices[0]}/${prices[1]}/${prices[2]} per million input/output/cached tokens"]
    if report["tasks_with_cost_for_every_harness"] != report["tasks"]:
        lines.append(f"   cost measures use the {report['tasks_with_cost_for_every_harness']} tasks where every harness reports a cost")
    lines.append(f"{'harness':<13}{'success':>8}{'95% interval':>15}{'$/attempt':>11}{'95% interval':>17}{'turns':>7}{'$/turn':>9}{'tokens':>10}  {'no usage':>8}")
    for label, row in rows.items():
        low, high = row["success_interval"]  # type: ignore[misc]
        d_low, d_high = row["dollars_interval"]  # type: ignore[misc]
        turns, per_turn, tokens = row["turns_per_attempt"], row["dollars_per_turn"], row["tokens_per_attempt"]
        lines.append(
            f"{label:<13}{_percent(row['success'])}{_percent(low):>9}-{_percent(high).strip():<6}{row['dollars_per_attempt']:>10.4f}  {d_low:>7.4f}-{d_high:<7.4f}"  # type: ignore[arg-type]
            f"{turns if turns is None else format(turns, '7.1f')}{'' if per_turn is None else format(per_turn, '9.5f')}{'' if tokens is None else format(tokens, '10.0f')}  {row['attempts_without_usage']:>8}"
        )
    frontier = report["pareto_frontier"]
    shares: dict[str, float] = report["frontier_share_of_resamples"]  # type: ignore[assignment]
    lines.append(f"\nPareto frontier (no harness is cheaper or equal and at least as successful): {', '.join(frontier)}")  # type: ignore[arg-type]
    lines.append("share of task resamples on the frontier: " + ", ".join(f"{label} {_percent(share).strip()}" for label, share in shares.items()))
    ratios: dict[str, tuple[float, float, float]] = report["cost_ratio_to_reference"]  # type: ignore[assignment]
    lines.append("cost of each harness over the reference: " + ", ".join(f"{label} {point:.2f}x ({low:.2f}-{high:.2f})" for label, (point, low, high) in ratios.items()))
    needed: dict[str, dict[str, int]] = report["tasks_to_resolve_against_reference"]  # type: ignore[assignment]
    lines.append("tasks to resolve a gap to the reference (95% interval of 2 points of success, or of 5% of cost; the spread of these runs, so a rough figure): " + ", ".join(f"{label} {n['success_gap_of_2_points']:,} / {n['cost_gap_of_5_percent']:,}" for label, n in needed.items()))
    effects: dict[str, float] = report["harness_effect_on_success"]  # type: ignore[assignment]
    lines.append("harness effect on success (rate minus the mean rate of all harnesses): " + ", ".join(f"{label} {100 * effect:+.1f} pt" for label, effect in effects.items()))
    lines.append(f"highest success: {', '.join(report['highest_success'])}; lowest cost: {', '.join(report['lowest_cost'])}")  # type: ignore[arg-type]
    own = report.get("own_harness")
    if isinstance(own, dict):
        lines.append(f"provider harness {own['label']}: highest success {own['has_highest_success']}, lowest cost {own['has_lowest_cost']}")
    unit = {"dollars": "${:.4f}", "turns": "{:.1f} turns", "tokens": "{:,.0f} tokens"}
    curves: dict[str, dict[str, object]] = report["cumulative_curves"]  # type: ignore[assignment]
    for measure, curve in curves.items():
        budgets: list[float] = curve["budgets"]  # type: ignore[assignment]
        lines.append(f"\ncumulative success rate within a spend per attempt of {', '.join(unit[measure].format(b) for b in budgets)}")
        within: dict[str, list[float]] = curve["success_within_budget"]  # type: ignore[assignment]
        for label, values in within.items():
            lines.append(f"  {label:<13}" + "".join(f"{_percent(value):>16}" for value in values))
    lines.append("\nfirst model call: tools, tool-schema characters, and instruction characters (every text but the task prompt) from one captured request;")
    lines.append(f"input tokens the provider reported for the first call (mean +- standard deviation); most turns in an attempt and attempts over the {TURN_CAP}-turn cap of the post")
    lines.append(f"  {'harness':<13}{'tools':>6}{'schema chars':>14}{'instruction chars':>19}{'first call tokens':>22}   turns")
    for label, row in rows.items():
        first = row["first_call_tokens"]
        tokens = "n/a" if first is None else f"{first[0]:,.0f} +- {first[1]:,.0f}"  # type: ignore[index]
        context = request_context.get(label)
        static = "     n/a           n/a                n/a" if context is None else f"{context['tools']:>6}{context['tool_schema_chars']:>14,}{context['instruction_chars']:>19,}"
        lines.append(f"  {label:<13}{static}{tokens:>22}   {row['most_turns_in_an_attempt']} / {row['attempts_over_turn_cap']}")
    return "\n".join(lines)


def main() -> int:
    from evals.harbor_wire import MODEL, PROFILES

    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--arm", action="append", required=True, help="LABEL=SOURCE:HARNESS; a repeated label adds attempts; the first label is the reference of the cost ratios")
    parser.add_argument("--audit", type=Path, action="append", default=[], help="a file that evals.audit_trials wrote; an invalid trial is not a pass")
    parser.add_argument("--tasks", type=Path, help="a file with one task name per line; every arm keeps only these tasks")
    parser.add_argument("--own", help="the label of the harness that belongs to the provider of the model")
    parser.add_argument("--name", default="benchmark")
    parser.add_argument("--resamples", type=int, default=RESAMPLES)
    parser.add_argument("--context", type=Path, help="a file with the tools, tool-schema characters, and instruction characters of the first request of each harness, under the key harnesses")
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
    arms = pool_sources([(label, load_rollouts(source, harness, audit, prices)) for label, source, harness in specs])
    if arguments.tasks:
        arms = restrict_to_tasks(arms, {line.strip().split("/")[-1] for line in arguments.tasks.read_text().splitlines() if line.strip()})
    reference = specs[0][0]
    try:
        report = analyze(arms, reference=reference, own=arguments.own, resamples=arguments.resamples)
    except ValueError as error:
        parser.error(str(error))
    request_context = {label: context for label, context in json.loads(arguments.context.read_text())["harnesses"].items() if label in arms} if arguments.context else {}
    arguments.output.write_text(json.dumps({"name": arguments.name, "prices_per_million_tokens": prices, "report": report, "request_context": request_context}, indent=2) + "\n")
    print(format_report(arguments.name, report, prices, request_context))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
