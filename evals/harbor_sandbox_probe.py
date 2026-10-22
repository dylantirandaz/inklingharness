"""A Harbor agent that calls no model and records what an agent could find in the sandbox.

For a SWE-bench task it writes key=value lines to moreprobe.txt in the agent logs, and
evals.validate_tasks reads them:

  task                                 the instance name
  base_commit_date                     the commit date of HEAD^, the base commit of the task
  refs, outside_commits                the refs of /testbed, and the commits that a ref holds
                                       and the history of HEAD does not
  outside_commits_not_older_than_base  the same commits when they are as new as the base
                                       commit, so that they could hold the fix
  package_dir=PATH                     a directory of the repository package, with an __init__.py,
                                       outside /testbed: another copy of the code under test
  archive=PATH                         a wheel or a source archive that names the package
  conda_pkgs_entries, pip_cache_kb     what is left of the package caches
  done=1                               the last line; a missing one means the probe stopped early

Use it in a job config as {"import_path": "evals.harbor_sandbox_probe:SandboxProbe", "model_name": ...,
"kwargs": {"tasks_dir": "out/terminal-bench/swe-bench-more"}} with the verifier disabled.
"""

from pathlib import Path
import re

from harbor.agents.capabilities import AgentCapabilities
from harbor.agents.installed.base import BaseInstalledAgent
from harbor.agents.model_connection import ModelConnectionSpec
from harbor.agents.options import InstalledAgentOptions

# The Python packages of each repository of SWE-bench Verified, by the owner in the instance name.
PACKAGES = {
    "astropy": ("astropy",),
    "django": ("django",),
    "matplotlib": ("matplotlib",),
    "mwaskom": ("seaborn",),
    "pallets": ("flask",),
    "psf": ("requests",),
    "pydata": ("xarray",),
    "pylint-dev": ("pylint",),
    "pytest-dev": ("_pytest", "pytest"),
    "scikit-learn": ("sklearn",),
    "sphinx-doc": ("sphinx",),
    "sympy": ("sympy",),
}

SCRIPT = r"""
set +e
echo "task=$(cat /tmp/moreprobe-task.txt)"
cd /testbed 2>/dev/null || { echo "no_testbed=1"; exit 0; }
base=$(git log -1 --format=%ct HEAD^ 2>/dev/null)
echo "base_commit_date=$base"
echo "refs=$(git for-each-ref | wc -l)"
echo "outside_commits=$(git rev-list --all --not HEAD 2>/dev/null | wc -l)"
echo "outside_commits_not_older_than_base=$(git rev-list --all --not HEAD --max-age=$base 2>/dev/null | wc -l)"
cd /
for name in $(cat /tmp/moreprobe-packages.txt); do
  find / -xdev \( -path /proc -o -path /sys -o -path /testbed -o -path /__modal \) -prune -o -type d -name "$name" -print 2>/dev/null | while read d; do [ -e "$d/__init__.py" ] && echo "package_dir=$d"; done
  find / -xdev \( -path /proc -o -path /sys -o -path /__modal \) -prune -o -type f \( -name "$name-*.whl" -o -name "$name-*.tar.gz" -o -name "$name-*.tar.bz2" -o -name "$name-*.conda" -o -name "$name-*.zip" \) -print 2>/dev/null | head -5 | sed 's/^/archive=/'
done
echo "conda_pkgs_entries=$(ls /opt/miniconda3/pkgs 2>/dev/null | wc -l)"
echo "pip_cache_kb=$(du -sk /root/.cache/pip 2>/dev/null | cut -f1)"
echo "done=1"
"""


def packages_of(instance: str) -> tuple[str, ...]:
    """The packages of the repository that an instance belongs to. An owner that is not listed is an error,
    because a probe that looked for no package would pass every task."""
    owner = instance.split("__")[0]
    if owner not in PACKAGES:
        raise ValueError(f"No package is known for the owner {owner!r} of {instance!r}; add it to PACKAGES")
    return PACKAGES[owner]


def normal(text: str) -> str:
    return re.sub(r"\s+", " ", text).strip()


def instance_of(instruction: str, tasks_dir: Path) -> str:
    """The instance whose instruction.md has this text. The agent gets the text of the task and not its name."""
    matches = [path.parent.name for path in tasks_dir.glob("*/instruction.md") if normal(path.read_text(errors="replace")) == normal(instruction)]
    if len(matches) != 1:
        raise ValueError(f"{len(matches)} tasks in {tasks_dir} have this instruction; expected one")
    return matches[0]


class SandboxProbeOptions(InstalledAgentOptions):
    tasks_dir: str


class SandboxProbe(BaseInstalledAgent):
    capabilities = AgentCapabilities()
    MODEL_CONNECTION = ModelConnectionSpec(default_provider="openrouter")
    options_model = SandboxProbeOptions
    options: SandboxProbeOptions

    @staticmethod
    def name() -> str:
        return "sandbox-probe"

    async def install(self, environment):
        pass

    def populate_context_post_run(self, context):
        pass

    async def _upload(self, environment, name: str, text: str) -> None:
        local = self.logs_dir / name
        local.write_text(text)
        await environment.upload_file(local, f"/tmp/{name}")

    async def run(self, instruction, environment, context):
        instance = instance_of(instruction, Path(self.options.tasks_dir))
        await self._upload(environment, "moreprobe-task.txt", instance + "\n")
        await self._upload(environment, "moreprobe-packages.txt", "\n".join(packages_of(instance)) + "\n")
        output = (self.environment_logs_dir / "moreprobe.txt").as_posix()
        await self.exec_as_agent(environment, command=f"({SCRIPT}) > {output} 2>&1")
