"""Tell a trial that the environment or the provider ended from a trial that the agent ended.

A fault says nothing about the harness, so its trial is archived and the same harness runs the same task
again. These are faults:

  - the sandbox did not set the agent up: a setup timeout, a failed install, a sandbox name that Modal refused
    because it was in use;
  - something outside the agent stopped it with SIGTERM (exit 143);
  - the provider failed during the trial, as evals.provider_health.PROVIDER_ERROR_PATTERN says. A harness that
    retried and went on is a fault as well, because its trial ran against the same failing provider.

Every other end is an outcome and counts, with its exception, because the harness took part in it: an agent that
ran out of time, one that crashed, one that the sandbox killed for memory (exit 137), and one that the provider
rejects every time, for example with a 422 after the agent attached an image, which Codex's view_image does on
this model. A trial that ended on an exception that this module does not know is unclassified, and so is a trial
without a grade. The tool lists it, exits 1, and leaves the decision to a person, who then adds the exception to
the right set here.

  python -m evals.trial_faults JOB...                 # list the faults; exit 1 when there is a fault or an unclassified trial
  python -m evals.trial_faults --archive DIR JOB...   # also move each fault trial to DIR/<job>/ and add it to DIR/manifest.json
"""

import argparse
from collections.abc import Sequence
from dataclasses import dataclass
import json
from pathlib import Path
import re
import shutil
import sys

from evals.provider_health import met_provider_error

# Harbor exception types that always mean a fault, with what to say.
FAULT_EXCEPTIONS = {
    "AgentSetupTimeoutError": "the sandbox did not set the agent up in time",
    "AlreadyExistsError": "Modal refused the sandbox name because it was in use",
}
# Types of exception that the agent takes part in. A fault can still hide in them: see judge_trial.
OUTCOME_EXCEPTIONS = frozenset({"AgentTimeoutError", "NonZeroAgentExitCodeError", "UnknownApiError"})
SIGTERM_EXIT_CODE = 143
_EXIT_CODE = re.compile(r"\(exit (\d+)\)")


@dataclass(frozen=True)
class Counted:
    """The trial ended by the agent, or without an exception. It counts."""


@dataclass(frozen=True)
class Fault:
    reason: str


@dataclass(frozen=True)
class Unclassified:
    reason: str


Verdict = Counted | Fault | Unclassified


def judge_trial(trial: Path) -> Verdict:
    result = json.loads((trial / "result.json").read_text())
    exception = result.get("exception_info")
    if exception is not None:
        kind, message = exception["exception_type"], exception.get("exception_message") or ""
        if kind in FAULT_EXCEPTIONS:
            return Fault(FAULT_EXCEPTIONS[kind])
        if kind == "RuntimeError":
            if message.startswith("Agent install failed"):
                return Fault("the agent install failed")
            return Unclassified(f"RuntimeError: {message[:80]}")
        if kind not in OUTCOME_EXCEPTIONS and kind != "ApiRateLimitError":
            return Unclassified(kind)
        exit_code = _EXIT_CODE.search(message)
        if kind == "NonZeroAgentExitCodeError" and exit_code is not None and int(exit_code.group(1)) == SIGTERM_EXIT_CODE:
            return Fault("something outside the agent stopped it with SIGTERM")
    if met_provider_error(trial):
        return Fault("the provider failed during the trial")
    if (result.get("verifier_result") or {}).get("rewards", {}).get("reward") is None:
        return Unclassified("the trial has no grade")
    return Counted()


@dataclass(frozen=True)
class Judged:
    job: Path
    trial: Path
    harness: str
    task: str
    verdict: Verdict


def judge_job(job: Path) -> list[Judged]:
    judged = []
    for result_path in sorted(job.glob("*__*/result.json")):
        result = json.loads(result_path.read_text())
        task = result["task_name"].rsplit("/", 1)[-1]
        judged.append(Judged(job, result_path.parent, result["agent_info"]["name"], task, judge_trial(result_path.parent)))
    return judged


def archive(faults: Sequence[Judged], directory: Path) -> None:
    """Move the trials of the faults to directory/<job name>/<trial>, and add them to directory/manifest.json."""
    manifest_path = directory / "manifest.json"
    manifest = json.loads(manifest_path.read_text()) if manifest_path.exists() else []
    for item in faults:
        assert isinstance(item.verdict, Fault)
        grade = (json.loads((item.trial / "result.json").read_text()).get("verifier_result") or {}).get("rewards", {}).get("reward")
        target = directory / item.job.name
        target.mkdir(parents=True, exist_ok=True)
        shutil.move(item.trial, target / item.trial.name)
        manifest.append({"job": item.job.name, "trial": item.trial.name, "harness": item.harness, "task": item.task, "grade": grade, "reason": item.verdict.reason})
    manifest_path.write_text(json.dumps(manifest, indent=1) + "\n")


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("jobs", nargs="+", type=Path)
    parser.add_argument("--archive", type=Path, metavar="DIR", help="move the fault trials to DIR and record them in DIR/manifest.json")
    arguments = parser.parse_args(argv)
    judged = [item for job in arguments.jobs for item in judge_job(job)]
    faults = [item for item in judged if isinstance(item.verdict, Fault)]
    unclassified = [item for item in judged if isinstance(item.verdict, Unclassified)]
    for item in faults + unclassified:
        label = "fault" if isinstance(item.verdict, Fault) else "UNCLASSIFIED"
        print(f"{label:<12} {item.job.name} {item.harness:<12} {item.task}  {item.trial.name.rsplit('__', 1)[-1]}  {item.verdict.reason}")
    print(f"{len(judged)} trials: {len(faults)} faults, {len(unclassified)} unclassified, {len(judged) - len(faults) - len(unclassified)} count")
    if arguments.archive is not None and faults:
        archive(faults, arguments.archive)
        print(f"archived {len(faults)} fault trials in {arguments.archive}; run these harness and task cells again:")
        for harness, task in sorted({(item.harness, item.task) for item in faults}):
            print(f"  {harness:<12} {task}")
    return 1 if faults or unclassified else 0


if __name__ == "__main__":
    sys.exit(main())
