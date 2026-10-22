"""Decide which SWE-bench tasks an airgapped run may use, from two checks that call no model.

A task is valid when all of these hold. Each check looks at the task and never at an
agent, so no result of an agent can choose the tasks.

  oracle    The reference solution passes the task in the closed environment.
  probe     In the agent phase, an agent can find no other copy of the code that it
            fixes. A copy is a directory of the repository package outside /testbed,
            or a wheel or an archive that names it. A later copy can hold the fix.
  history   No commit that the history of HEAD lacks is as new as the base commit.
            A commit like that can hold the fix. Older ones, such as a release tag
            from before the issue, cannot.

The probe is evals.harbor_sandbox_probe, a Harbor agent that calls no model. Its output format is in its docstring.

  python -m evals.validate_tasks --order evals/swe-more-order.json --through 240 \\
      --oracle out/terminal-bench/jobs/ORACLE --probe out/terminal-bench/jobs/PROBE \\
      --output evals/swe-more-valid.json
"""

import argparse
from dataclasses import dataclass
import json
from pathlib import Path
from typing import Literal

Reason = Literal[
    "oracle_missing",
    "oracle_failed",
    "probe_missing",
    "probe_incomplete",
    "package_copy_outside_testbed",
    "archive_of_the_package",
    "newer_commits_outside_head",
]


@dataclass(frozen=True)
class Probe:
    """What the probe found in one task. complete is false when it stopped before its last line."""

    complete: bool
    outside_commits_not_older_than_base: int | None
    package_dirs: tuple[str, ...]
    archives: tuple[str, ...]


def parse_probe(text: str) -> tuple[str, Probe]:
    """The task name and the findings of one probe output."""
    task = ""
    newer: int | None = None
    complete = False
    package_dirs: list[str] = []
    archives: list[str] = []
    for line in text.splitlines():
        key, separator, value = line.partition("=")
        if not separator:
            continue
        match key:
            case "task":
                task = value.strip()
            case "outside_commits_not_older_than_base" if value.strip().isdigit():
                newer = int(value)
            case "package_dir":
                package_dirs.append(value.strip())
            case "archive":
                archives.append(value.strip())
            case "done":
                complete = value.strip() == "1"
            case _:
                pass  # The other keys are counts for the record.
    if not task:
        raise ValueError("A probe output names no task")
    return task, Probe(complete=complete, outside_commits_not_older_than_base=newer, package_dirs=tuple(package_dirs), archives=tuple(archives))


def reasons_to_exclude(oracle_passed: bool | None, probe: Probe | None) -> tuple[Reason, ...]:
    """Why a task is not valid. None for a check means that the check did not run. An empty result means valid."""
    reasons: list[Reason] = []
    if oracle_passed is None:
        reasons.append("oracle_missing")
    elif not oracle_passed:
        reasons.append("oracle_failed")
    if probe is None:
        reasons.append("probe_missing")
    elif not probe.complete or probe.outside_commits_not_older_than_base is None:
        reasons.append("probe_incomplete")
    else:
        if probe.package_dirs:
            reasons.append("package_copy_outside_testbed")
        if probe.archives:
            reasons.append("archive_of_the_package")
        if probe.outside_commits_not_older_than_base > 0:
            reasons.append("newer_commits_outside_head")
    return tuple(reasons)


@dataclass(frozen=True)
class Validation:
    valid: tuple[str, ...]
    excluded: dict[str, tuple[Reason, ...]]


def validate(order: list[str], oracle: dict[str, bool], probes: dict[str, Probe]) -> Validation:
    """Keep the order of the tasks. A task that a check never reached is excluded, not guessed valid."""
    valid: list[str] = []
    excluded: dict[str, tuple[Reason, ...]] = {}
    for task in order:
        reasons = reasons_to_exclude(oracle.get(task), probes.get(task))
        if reasons:
            excluded[task] = reasons
        else:
            valid.append(task)
    return Validation(tuple(valid), excluded)


def read_oracle(job: Path) -> dict[str, bool]:
    """The tasks of an oracle job, with whether the reference solution passed."""
    passed: dict[str, bool] = {}
    for result_path in sorted(job.glob("*__*/result.json")):
        result = json.loads(result_path.read_text())
        reward = ((result.get("verifier_result") or {}).get("rewards") or {}).get("reward")
        passed[result["task_name"].split("/")[-1]] = reward == 1.0
    return passed


def read_probes(job: Path) -> dict[str, Probe]:
    probes: dict[str, Probe] = {}
    for output in sorted(job.glob("*__*/agent/moreprobe.txt")):
        task, probe = parse_probe(output.read_text(errors="replace"))
        probes[task] = probe
    return probes


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--order", type=Path, required=True, help="a JSON file whose order key lists the task names in the order of use")
    parser.add_argument("--through", type=int, required=True, help="the number of leading tasks of the order that the two checks covered")
    parser.add_argument("--oracle", type=Path, action="append", required=True)
    parser.add_argument("--probe", type=Path, action="append", required=True)
    parser.add_argument("--output", type=Path, required=True)
    arguments = parser.parse_args()
    order = json.loads(arguments.order.read_text())["order"][: arguments.through]
    oracle: dict[str, bool] = {}
    for job in arguments.oracle:
        oracle.update(read_oracle(job))
    probes: dict[str, Probe] = {}
    for job in arguments.probe:
        probes.update(read_probes(job))
    validation = validate(order, oracle, probes)
    arguments.output.write_text(json.dumps({"covered": len(order), "valid": list(validation.valid), "excluded": {task: list(reasons) for task, reasons in validation.excluded.items()}}, indent=1) + "\n")
    print(f"{len(validation.valid)} of {len(order)} tasks are valid; {len(validation.excluded)} excluded")
    reasons_seen: dict[str, int] = {}
    for reasons in validation.excluded.values():
        for reason in reasons:
            reasons_seen[reason] = reasons_seen.get(reason, 0) + 1
    for reason, count in sorted(reasons_seen.items(), key=lambda item: -item[1]):
        print(f"  {count:>4}  {reason}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
