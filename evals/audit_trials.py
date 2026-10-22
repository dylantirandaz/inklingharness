"""Audit finished trials for leakage and cheating, from what each agent ran.

An invalid trial is one whose transcript shows that the agent

  used_model_api     called the model API from its own shell, for example with the
                     key in its environment. That can reach another model, or the
                     web search that the API adds to any model;
  called_web_tool    used a web search or web fetch tool;
  retrieved_answer   received the answer. On SWE-bench, a fetch or a web tool
                     returned at least half of the added lines of the reference
                     patch. On Terminal-Bench, a fetch returned a file of the
                     benchmark repository that holds a solution, tests, or a task.

A noted trial is one that tried something that is not an error of itself and that
a closed environment stops: network_attempt, searched_credentials. A trial can
be both. Counting invalid trials as failed removes any gain from cheating.

  python -m evals.audit_trials --output audit.json JOB_DIRECTORY ...
"""

import argparse
from collections import Counter
from dataclasses import dataclass
import json
from pathlib import Path
import re
from typing import Literal

Invalid = Literal["used_model_api", "called_web_tool", "retrieved_answer"]
Noted = Literal["network_attempt", "searched_credentials"]

WEB_TOOLS = re.compile(r"^(WebSearch|WebFetch|web_search|web_fetch|web_search_call)$")
# What opens the model API: its host, a literal key, or the variables that hold the key
# or the base URL of the harness. A harness never mentions these in its tool calls, so
# any mention is the agent reaching for the key. Generic paths such as /v1/models and
# authorization headers are not evidence, because other services use them too.
MODEL_API = re.compile(r"openrouter\.ai|sk-or-v1-|OPENROUTER_API_KEY|ANTHROPIC_API_KEY|ANTHROPIC_AUTH_TOKEN|OPENAI_API_KEY|ANTHROPIC_BASE_URL|OPENAI_BASE_URL", re.I)
CREDENTIAL_SEARCH = re.compile(r"(printenv|\benv\b|os\.environ|/proc/\S*/environ|\bset\b).*(key|token|secret|api|openai|anthropic|openrouter)|(key|token|secret|api|openai|anthropic|openrouter).*(printenv|\benv\b|os\.environ)", re.I)
NETWORK_COMMAND = re.compile(r"\b(curl|wget)\b|git\s+(fetch|pull|clone|ls-remote)|pip3?\s+(download|install)|python3?\s+-m\s+pip\s+(download|install)|npm\s+(install|i)\b|apt(-get)?\s+(install|update)|conda\s+install|https?://|urllib|requests\.(get|post)", re.I)
# A page of the benchmark repository that holds a solution, a task description, or tests.
BENCHMARK_FILE = re.compile(r"(laude-institute|harbor-framework)/terminal-bench[\w.-]*|terminal-bench-2(-1)?/(blob|tree|raw)|/tasks/[\w.-]+/(solution|tests)/|solution\.sh|solve\.sh", re.I)
FETCH_HOST = re.compile(r"github\.com|githubusercontent\.com", re.I)
# Tools that read or write files. Their arguments hold file text, and code can hold requests.get or a URL without
# any fetch: Claude Code's Edit of sphinx/builders/linkcheck.py did, and the result of the Edit echoed the line
# that the agent added, which is also a line of the reference patch. A tool that is not here may run a command,
# so the audit takes it for a possible fetch.
FILE_TOOLS = re.compile(r"^(read|Read|read_file|edit|Edit|edit_file|write|Write|write_file|apply_patch|MultiEdit)$")
# What a failed fetch returns: not an answer.
FAILED_FETCH = re.compile(r"^(cannot|no fetch|no remote|could not|curl: \(|fatal:)|Process exited with code [1-9]|\b404\b.{0,20}Not Found|Request failed|\[exit code [1-9]", re.I)
MIN_PAGE = 300
ANSWER_SHARE = 0.5
MIN_LINE = 18


@dataclass(frozen=True)
class Step:
    """One tool call of an agent and what came back. An empty result means none came."""

    tool: str
    arguments: str
    result: str


@dataclass(frozen=True)
class Verdict:
    invalid: frozenset[Invalid]
    noted: frozenset[Noted]

    @property
    def is_invalid(self) -> bool:
        return bool(self.invalid)


def _text(value: object) -> str:
    return value if isinstance(value, str) else json.dumps(value, ensure_ascii=False)


def _think_steps(directory: Path) -> list[Step]:
    pending: list[tuple[str, str]] = []
    steps: list[Step] = []
    for line in (directory / "agent" / "think-events.jsonl").read_text(errors="replace").splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get("type") == "tool_call":
            pending.append((_text(event.get("name")), _text(event.get("input"))))
        elif event.get("type") == "tool_result" and pending:
            tool, arguments = pending.pop(0)
            steps.append(Step(tool, arguments, _text(event.get("content"))))
    steps.extend(Step(tool, arguments, "") for tool, arguments in pending)
    return steps


def _bare_steps(directory: Path) -> list[Step]:
    steps: list[Step] = []
    for line in (directory / "agent" / "bare-events.jsonl").read_text(errors="replace").splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get("event") == "tool":
            if "input" not in event:
                raise ValueError(f"{directory} was run with a build of bare that does not log commands; it cannot be audited")
            steps.append(Step("bash", _text(event["input"]), _text(event.get("output"))))
    return steps


def _atif_steps(directory: Path) -> list[Step]:
    trajectory = json.loads((directory / "agent" / "trajectory.json").read_text())
    steps: list[Step] = []
    for step in trajectory.get("steps", []):
        results = {item.get("source_call_id"): _text(item.get("content")) for item in (step.get("observation") or {}).get("results", [])}
        for call in step.get("tool_calls") or []:
            steps.append(Step(_text(call.get("function_name")), _text(call.get("arguments")), results.get(call.get("tool_call_id"), "")))
    return steps


def read_steps(arm: str, directory: Path) -> list[Step]:
    """The tool calls of a trial. Think and bare write their own event files; the other harnesses write an ATIF trajectory."""
    match arm:
        case "think":
            return _think_steps(directory)
        case "bare":
            return _bare_steps(directory)
        case "claude-code" | "codex" | "pi":
            return _atif_steps(directory)
        case _:
            raise ValueError(f"No transcript reader for the harness {arm!r}")


def _normal(text: str) -> str:
    return re.sub(r"\s+", " ", text)


def answer_lines(solve_script: str) -> list[str]:
    """The added lines of a reference patch, long enough to be telling."""
    lines = [_normal(raw[1:]).strip() for raw in solve_script.splitlines() if raw.startswith("+") and not raw.startswith("+++")]
    return [line for line in lines if len(line) >= MIN_LINE]


def _returned_a_page(step: Step) -> bool:
    return len(step.result) >= MIN_PAGE and not FAILED_FETCH.search(step.result[:200])


def audit_steps(steps: list[Step], *, patch_lines: list[str] | None, benchmark: Literal["swe-bench", "terminal-bench"]) -> Verdict:
    invalid: set[Invalid] = set()
    noted: set[Noted] = set()
    for step in steps:
        if WEB_TOOLS.match(step.tool):
            invalid.add("called_web_tool")
        if MODEL_API.search(step.arguments):
            invalid.add("used_model_api")
        elif CREDENTIAL_SEARCH.search(step.arguments):
            noted.add("searched_credentials")
        if NETWORK_COMMAND.search(step.arguments):
            noted.add("network_attempt")
        if not _returned_a_page(step):
            continue
        fetched = bool(WEB_TOOLS.match(step.tool) or (NETWORK_COMMAND.search(step.arguments) and not FILE_TOOLS.match(step.tool)))
        if benchmark == "swe-bench" and patch_lines and fetched:
            page = _normal(step.result)
            if sum(line in page for line in patch_lines) / len(patch_lines) >= ANSWER_SHARE:
                invalid.add("retrieved_answer")
        elif benchmark == "terminal-bench" and fetched and FETCH_HOST.search(step.arguments) and BENCHMARK_FILE.search(step.arguments):
            invalid.add("retrieved_answer")
    return Verdict(frozenset(invalid), frozenset(noted))


def _patch_lines_for(task: str, roots: list[Path]) -> list[str] | None:
    instance = task.split("/")[-1]
    for root in roots:
        for solve in sorted(root.glob(f"{instance}/solution/solve.sh")) + sorted(root.glob(f"{instance}/*/solution/solve.sh")):
            return answer_lines(solve.read_text(errors="replace"))
    return None


def audit_job(job: Path, roots: list[Path]) -> dict[str, dict[str, object]]:
    audited: dict[str, dict[str, object]] = {}
    for result_path in sorted(job.glob("*__*/result.json")):
        result = json.loads(result_path.read_text())
        arm, task = result["agent_info"]["name"], result["task_name"]
        if not (result_path.parent / "agent").is_dir() or arm not in ("think", "bare", "claude-code", "codex", "pi"):
            continue
        benchmark: Literal["swe-bench", "terminal-bench"] = "swe-bench" if task.startswith("swe-bench/") else "terminal-bench"
        patch = _patch_lines_for(task, roots) if benchmark == "swe-bench" else None
        if benchmark == "swe-bench" and patch is None:
            raise FileNotFoundError(f"No reference patch for {task} under {[str(root) for root in roots]}")
        try:
            verdict = audit_steps(read_steps(arm, result_path.parent), patch_lines=patch, benchmark=benchmark)
        except FileNotFoundError:
            continue  # The trial ended before its harness wrote a transcript, so it has no tool calls to audit.
        audited[f"{job.name}/{result_path.parent.name}"] = {"arm": arm, "task": task, "invalid": sorted(verdict.invalid), "noted": sorted(verdict.noted)}
    return audited


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("jobs", nargs="+", type=Path)
    parser.add_argument("--task-root", action="append", type=Path, default=[], help="a folder of tasks that holds <task>/solution/solve.sh; repeat. Default: the Harbor SWE-bench cache")
    parser.add_argument("--output", type=Path, required=True)
    arguments = parser.parse_args()
    roots = arguments.task_root or [Path.home() / ".cache/harbor/tasks/packages/swe-bench"]
    audited: dict[str, dict[str, object]] = {}
    for job in arguments.jobs:
        audited.update(audit_job(job, roots))
    arguments.output.write_text(json.dumps(audited, indent=2) + "\n")
    by_arm: dict[str, Counter[str]] = {}
    for entry in audited.values():
        counts = by_arm.setdefault(str(entry["arm"]), Counter())
        counts["trials"] += 1
        counts["INVALID"] += bool(entry["invalid"])
        for flag in [*entry["invalid"], *entry["noted"]]:  # type: ignore[misc]
            counts[flag] += 1
    columns = ["trials", "INVALID", "used_model_api", "called_web_tool", "retrieved_answer", "network_attempt", "searched_credentials"]
    print(f"{'arm':<13}" + "".join(f"{column:>21}" for column in columns))
    for arm, counts in sorted(by_arm.items()):
        print(f"{arm:<13}" + "".join(f"{counts[column]:>21}" for column in columns))
    return 1 if any(entry["invalid"] for entry in audited.values()) else 0


if __name__ == "__main__":
    raise SystemExit(main())
