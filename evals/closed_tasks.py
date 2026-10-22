"""Copy Harbor tasks and close the network of their agent phase.

Some benchmark tasks have answers on public sites, and an agent with a
network can fetch them. Harbor reads a network policy for the agent phase from
the [agent] table of task.toml, and a run-level option cannot add one to a
task that allows the public network. This tool therefore writes local copies
of the tasks with an allowlist in [agent].

The environment baseline stays as the task sets it, so a harness can still
install itself before the agent starts. Only agent.run() is limited, to the
hosts that you name. Name the model API and nothing else.

With the network closed, an agent cannot install a missing package. A SWE-bench
image activates its testbed environment in .bashrc, so only a login or an
interactive shell gets the repository's dependencies, and a plain `bash -c`
gets the base Python without them. Harnesses start bash in different ways, and
with the network open they all worked around the gap with pip. For SWE-bench,
--swe-image therefore puts the testbed environment first on the PATH of the
image, so every harness gets the same Python. It also deletes the conda package
cache and the pip cache. The conda cache of an xarray image holds xarray 2025.4.0,
which contains the fix of two tasks, and nothing needs the cache at run time.

Harbor caches tasks in two layouts. SWE-bench tasks are packages:
CACHE/<task>/<digest>/task.toml. Terminal-Bench 2.0 tasks come from git:
CACHE/<id>/<task>/task.toml.

  python -m evals.closed_tasks --layout packages --swe-image \
      --source ~/.cache/harbor/tasks/packages/swe-bench \
      --config evals/swe-bench-native.json \
      --dest out/terminal-bench/swe-bench-closed --allow-host openrouter.ai
  python -m evals.closed_tasks --layout git --source ~/.cache/harbor/tasks \
      --dest out/terminal-bench/tb-closed --allow-host openrouter.ai
"""

import argparse
import json
from pathlib import Path
import re
import shutil
import sys
from typing import Literal

from harbor.models.task.config import NetworkMode, TaskConfig

Layout = Literal["packages", "git"]
_AGENT_TABLE = re.compile(r"^\[agent\][ \t]*$", re.MULTILINE)
# The PATH of every SWE-bench image, with its testbed environment in front.
_TESTBED_PATH = "ENV PATH=/opt/miniconda3/envs/testbed/bin:/opt/miniconda3/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
_REMOVE_CACHES = "RUN rm -rf /opt/miniconda3/pkgs /root/.cache/pip"


def close_agent_network(task_toml: str, allowed_hosts: list[str]) -> str:
    """Return task_toml with an allowlist for the agent phase.

    It refuses a task that already sets an agent network policy, and it checks
    the result with Harbor: the agent phase must be limited to exactly these
    hosts, and the environment baseline must not change."""
    if not allowed_hosts:
        raise ValueError("Name at least one allowed host; a closed agent phase cannot reach the model")
    before = TaskConfig.model_validate_toml(task_toml)
    if before.agent.network_mode is not None or before.agent.allowed_hosts is not None:
        raise ValueError("The task already sets an agent network policy")
    policy = f'network_mode = "allowlist"\nallowed_hosts = {json.dumps(allowed_hosts)}'
    table = _AGENT_TABLE.search(task_toml)
    if table:
        # The keys of a TOML table can come in any order, so put these first.
        closed = task_toml[: table.end()] + "\n" + policy + task_toml[table.end():]
    else:
        closed = task_toml.rstrip("\n") + "\n\n[agent]\n" + policy + "\n"
    after = TaskConfig.model_validate_toml(closed)
    agent_policy = after.agent.explicit_phase_policy()
    if agent_policy is None or agent_policy.network_mode != NetworkMode.ALLOWLIST or agent_policy.allowed_hosts != allowed_hosts:
        raise ValueError("The closed task does not give the agent phase the requested allowlist")
    if after.environment.resolve_baseline() != before.environment.resolve_baseline():
        raise ValueError("Closing the agent phase changed the environment baseline")
    return closed


def put_testbed_first(dockerfile: str) -> str:
    """Return dockerfile with the testbed environment first on the PATH."""
    if re.search(r"^ENV\s+PATH[ =]", dockerfile, re.MULTILINE):
        raise ValueError("The Dockerfile already sets PATH")
    return dockerfile.rstrip("\n") + "\n" + _TESTBED_PATH + "\n"


def remove_package_caches(dockerfile: str) -> str:
    """Return dockerfile with a last step that deletes the conda and pip caches."""
    return dockerfile.rstrip("\n") + "\n" + _REMOVE_CACHES + "\n"


def find_tasks(source: Path, layout: Layout) -> dict[str, Path]:
    """Map each task name to its folder. A name with two folders is an error."""
    found: dict[str, Path] = {}
    for toml in sorted(source.glob("*/*/task.toml")):
        folder = toml.parent
        name = folder.parent.name if layout == "packages" else folder.name
        if name in found:
            raise ValueError(f"Two cached versions of {name}: {found[name]} and {folder}")
        found[name] = folder
    return found


def copy_closed(tasks: dict[str, Path], names: list[str], destination: Path, allowed_hosts: list[str], *, swe_image: bool) -> None:
    missing = [name for name in names if name not in tasks]
    if missing:
        raise ValueError(f"No cached task for {len(missing)} names, for example {missing[0]!r}")
    destination.mkdir(parents=True, exist_ok=True)
    for name in names:
        target = destination / name
        if target.exists():
            raise FileExistsError(f"{target} exists; remove it to copy again")
        source = tasks[name]
        shutil.copytree(source, target)
        (target / "task.toml").write_text(close_agent_network((source / "task.toml").read_text(), allowed_hosts))
        if swe_image:
            dockerfile = target / "environment" / "Dockerfile"
            dockerfile.write_text(remove_package_caches(put_testbed_first(dockerfile.read_text())))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--layout", choices=["packages", "git"], required=True)
    parser.add_argument("--source", type=Path, required=True, help="the Harbor cache folder that holds the tasks")
    parser.add_argument("--config", type=Path, help="a Harbor job config whose first dataset lists task_names; without it, every cached task is copied")
    parser.add_argument("--dest", type=Path, required=True)
    parser.add_argument("--allow-host", action="append", required=True, dest="hosts")
    parser.add_argument("--swe-image", action="store_true", help="SWE-bench: put the testbed environment first on the PATH of each image and delete the conda and pip caches")
    arguments = parser.parse_args()
    tasks = find_tasks(arguments.source.expanduser(), arguments.layout)
    if arguments.config:
        names = [name.split("/")[-1] for name in json.loads(arguments.config.read_text())["datasets"][0]["task_names"]]
    else:
        names = sorted(tasks)
    copy_closed(tasks, names, arguments.dest, arguments.hosts, swe_image=arguments.swe_image)
    print(f"Wrote {len(names)} closed tasks to {arguments.dest}; the agent phase may reach {', '.join(arguments.hosts)}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
