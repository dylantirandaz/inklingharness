"""Say how each trial ended, so that a person can read the failures by kind.

Run it on a pilot before a real run. A harness and a model that do not fit each other show up here:
on October 20 a tool call that the model wrote as text ended 16 Claude Code runs, and Codex ended
7 runs on a 422 after it showed an image to the model. Each kind has a different remedy.

  python -m evals.failure_taxonomy JOB...                # the kinds of trial by harness
  python -m evals.failure_taxonomy --list KIND JOB...    # the trials of one kind

A trial has the first kind that fits, in this order:

  passed             the grade is 1
  fault              the environment or the provider ended it (evals.trial_faults), so it must run again
  unclassified       an exception that evals.trial_faults does not know, or no grade; a person decides
  timed_out          the agent ran out of time
  tool_call_as_text  the end of the agent log holds a tool call that the model wrote as text
  provider_rejected  the end of the agent log holds a 422 that the provider repeats for the same request
  no_edit            the agent ended without changing a file
  broke_other_tests  SWE-bench: the tests that passed before fail (the tests of the fix may fail too)
  wrong_fix          SWE-bench: the tests of the fix fail and the other tests pass
  failed             any other failure, for example in Terminal-Bench
"""

import argparse
from collections import Counter
from collections.abc import Sequence
from dataclasses import dataclass
import json
from pathlib import Path
import re
from typing import Literal, get_args

from evals.audit_trials import read_steps
from evals.trial_faults import Counted, Fault, Unclassified, judge_trial

Kind = Literal[
    "passed",
    "fault",
    "unclassified",
    "timed_out",
    "tool_call_as_text",
    "provider_rejected",
    "no_edit",
    "broke_other_tests",
    "wrong_fix",
    "failed",
]
KINDS: tuple[Kind, ...] = get_args(Kind)

# The part of the agent log that holds the last replies. A tool call written as text in the middle of a run that went on is not the end.
LOG_TAIL_CHARACTERS = 8000
TOOL_CALL_AS_TEXT = re.compile(r'\\?"name\\?"\s*:\s*\\?"[A-Za-z_]+\\?"\s*,\s*\\?"args\\?"\s*:\s*\{')
PROVIDER_REJECTION = re.compile(r"(?:unexpected status|answered|API Error:) 422\b")
FILE_EDIT_TOOLS = re.compile(r"^(edit|Edit|edit_file|write|Write|write_file|apply_patch|MultiEdit)$")
SHELL_TOOLS = re.compile(r"^(bash|Bash|exec_command|write_stdin)$")
SHELL_WRITES = re.compile(r"\bsed\s+-i|\bgit\s+apply|\bpatch\b|cat\s*>|\btee\b|>>|python3?\s+-\s*<<|\bperl\s+-pi|open\(.+['\"]w['\"]|write_text|\.write\(|\bapply_patch\b", re.I)


@dataclass(frozen=True)
class Classified:
    job: Path
    trial: Path
    harness: str
    task: str
    kind: Kind


def _log_tail(trial: Path) -> str:
    parts = []
    for path in sorted((trial / "agent").glob("*")):
        if path.is_file() and path.suffix in (".txt", ".log", ".jsonl"):
            parts.append(path.read_text(errors="replace")[-LOG_TAIL_CHARACTERS:])
    return "\n".join(parts)


def _edited(trial: Path, harness: str) -> bool | None:
    """Whether the agent changed a file. None when the transcript cannot be read."""
    try:
        steps = read_steps(harness, trial)
    except (OSError, ValueError, KeyError):
        return None
    return any(FILE_EDIT_TOOLS.match(step.tool) or (SHELL_TOOLS.match(step.tool) and SHELL_WRITES.search(step.arguments)) for step in steps)


def _swe_bench_kind(trial: Path) -> Kind | None:
    """broke_other_tests or wrong_fix from the SWE-bench report of the verifier. None when the trial has no such report."""
    report_path = trial / "verifier" / "report.json"
    if not report_path.exists():
        return None
    report = next(iter(json.loads(report_path.read_text()).values()))
    status = report.get("tests_status", {})
    if status.get("PASS_TO_PASS", {}).get("failure"):
        return "broke_other_tests"
    return "wrong_fix" if status.get("FAIL_TO_PASS", {}).get("failure") else None


def classify_trial(trial: Path) -> Kind:
    result = json.loads((trial / "result.json").read_text())
    if (result.get("verifier_result") or {}).get("rewards", {}).get("reward") == 1.0:
        return "passed"
    match judge_trial(trial):
        case Fault():
            return "fault"
        case Unclassified():
            return "unclassified"
        case Counted():
            pass
    if (result.get("exception_info") or {}).get("exception_type") == "AgentTimeoutError":
        return "timed_out"
    tail = _log_tail(trial)
    if TOOL_CALL_AS_TEXT.search(tail):
        return "tool_call_as_text"
    if PROVIDER_REJECTION.search(tail):
        return "provider_rejected"
    if _edited(trial, result["agent_info"]["name"]) is False:
        return "no_edit"
    return _swe_bench_kind(trial) or "failed"


def classify_job(job: Path) -> list[Classified]:
    classified = []
    for result_path in sorted(job.glob("*__*/result.json")):
        result = json.loads(result_path.read_text())
        classified.append(Classified(job, result_path.parent, result["agent_info"]["name"], result["task_name"].rsplit("/", 1)[-1], classify_trial(result_path.parent)))
    return classified


def table(items: Sequence[Classified]) -> str:
    counts: dict[str, Counter[Kind]] = {}
    for item in items:
        counts.setdefault(item.harness, Counter())[item.kind] += 1
    shown = [kind for kind in KINDS if any(counter[kind] for counter in counts.values())]
    lines = [f"{'harness':<14}" + "".join(f"{kind:>19}" for kind in shown) + f"{'trials':>8}"]
    for harness, counter in sorted(counts.items()):
        lines.append(f"{harness:<14}" + "".join(f"{counter[kind]:>19}" for kind in shown) + f"{sum(counter.values()):>8}")
    return "\n".join(lines)


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("jobs", nargs="+", type=Path)
    parser.add_argument("--list", choices=KINDS, help="print the trials of this kind")
    arguments = parser.parse_args(argv)
    items = [item for job in arguments.jobs for item in classify_job(job)]
    if arguments.list:
        for item in items:
            if item.kind == arguments.list:
                print(f"{item.job.name} {item.harness:<12} {item.task}  {item.trial.name.rsplit('__', 1)[-1]}")
    else:
        print(table(items))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
