"""The task splits of the benchmark, and the check that keeps the final test sealed.

A harness must not be tuned on the tasks that test it. Each task is in exactly one split:

  dev         tasks that an agent has run on before the protocol (the 38 SWE-bench and 53 Terminal-Bench
              tasks of the airgapped runs). Think was designed while these runs were read.
  round1      the 232 SWE-bench tasks of round 1. They were held out for the frozen Think build
              af9108f360ff.... Once a later Think build is tuned with them, they are dev for that build.
  final_test  SWE-bench tasks that no agent has run on. The two checks that choose them call no model.
              They are used once, for the final comparison of the harnesses that the protocol names.

  python -m evals.splits --check   # exit 1 when a split overlaps or an agent has run on a final-test task
"""

import argparse
from collections.abc import Sequence
from dataclasses import dataclass
import json
from pathlib import Path

SPLITS_PATH = Path(__file__).with_name("splits.json")
# The agents that call no model: the reference solution and the sandbox probe. Any other agent on a
# final-test task is a violation, including an agent that this list does not know.
NO_MODEL_AGENTS = frozenset({"oracle", "moreprobe"})


@dataclass(frozen=True)
class Splits:
    dev: frozenset[str]
    round1: frozenset[str]
    final_test: frozenset[str]
    # Tasks of a split that are long or large, chosen from the task metadata and never from a result.
    final_test_stress: frozenset[str]
    round1_stress: frozenset[str]


def load(path: Path = SPLITS_PATH) -> Splits:
    raw = json.loads(path.read_text())
    return Splits(*(frozenset(raw[key]) for key in ("dev", "round1", "final_test", "final_test_stress", "round1_stress")))


def overlaps(splits: Splits) -> dict[str, frozenset[str]]:
    """The tasks that are in two splits, by the names of the two splits. An empty result means that the splits are disjoint.
    The stress lists must be inside their splits, and a stress task outside its split is reported as well."""
    pairs = {
        "dev and round1": splits.dev & splits.round1,
        "dev and final_test": splits.dev & splits.final_test,
        "round1 and final_test": splits.round1 & splits.final_test,
        "final_test_stress outside final_test": splits.final_test_stress - splits.final_test,
        "round1_stress outside round1": splits.round1_stress - splits.round1,
    }
    return {name: tasks for name, tasks in pairs.items() if tasks}


def ran_on_final_test(splits: Splits, jobs_dir: Path) -> list[str]:
    """One line for each trial of an agent that calls a model on a final-test task, in any job folder."""
    found = []
    for result_path in sorted(jobs_dir.glob("**/*__*/result.json")):
        result = json.loads(result_path.read_text())
        task = result["task_name"].rsplit("/", 1)[-1]
        harness = result["agent_info"]["name"]
        if task in splits.final_test and harness not in NO_MODEL_AGENTS:
            found.append(f"{result_path.parent.relative_to(jobs_dir)} ({harness}) ran on the final-test task {task}")
    return found


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--check", action="store_true", required=True)
    parser.add_argument("--splits", type=Path, default=SPLITS_PATH)
    parser.add_argument("--jobs", type=Path, default=Path("out/terminal-bench/jobs"))
    arguments = parser.parse_args(argv)
    splits = load(arguments.splits)
    problems = [f"{name}: {sorted(tasks)[:3]} ({len(tasks)} tasks)" for name, tasks in overlaps(splits).items()] + ran_on_final_test(splits, arguments.jobs)
    for problem in problems:
        print(problem)
    print(f"dev {len(splits.dev)}, round1 {len(splits.round1)}, final_test {len(splits.final_test)} (stress {len(splits.final_test_stress)}): {'sealed' if not problems else f'{len(problems)} problems'}")
    return 1 if problems else 0


if __name__ == "__main__":
    raise SystemExit(main())
