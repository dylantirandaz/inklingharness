"""Pinned native Pi/OMP launches; the shared wire boundary owns usage and policy.

Pi and OMP retain native retry and compaction behavior. OMP model fallback is
explicitly disabled; neither adapter adds a turn limit or a task timeout.
Task images need Bash and their installed Node/Bun runtime, not Python.
"""

import logging
import shlex
from pathlib import Path

from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

from evals.harbor_wire import (
    MODEL,
    exec_wire,
    install_wire,
    populate_wire_context,
    validate_linux_binary,
)


_LOGS = "/logs/agent"
_PI_VERSION = "0.87.1"
_NODE_VERSION = "22.19.0"
_OMP_VERSION = "18.6.1"
_BUN_VERSION = "1.3.14"
_PI_BIN = f"/installed-agent/pi-nvm/versions/node/v{_NODE_VERSION}/bin"
_OMP_BIN = "/installed-agent/omp-bun/bin"
_MODEL_SELECTOR = f"inkling-eval/{MODEL}"


def _isolated_env(peer: str) -> dict[str, str]:
    home = f"{_LOGS}/{peer}-home"
    return {
        "PI_CODING_AGENT_DIR": f"{home}/agent",
        "PI_OFFLINE": "1",
        "PI_TELEMETRY": "0",
    }


def _launch_command(arguments: list[str], bin_dir: str) -> str:
    # PATH expansion happens in the task shell, never on the controller host.
    return (
        f'export PATH={shlex.quote(bin_dir)}:"$PATH"; '
        "exec /installed-agent/harbor-wire "
        f"-logs {_LOGS}/wire -messages-policy normalize -- "
        f"{shlex.join(arguments)} </dev/null "
        f">{_LOGS}/events.jsonl 2>{_LOGS}/stderr.log"
    )


class PiAgent(BaseInstalledAgent):
    """Stock Pi 0.87.1 with a per-trial Messages provider registration."""

    def __init__(
        self,
        logs_dir: Path,
        model_name: str,
        *,
        wire_binary_path: str | Path,
        logger: logging.Logger | None = None,
        extra_env: dict[str, str] | None = None,
        version: str | None = None,
    ) -> None:
        if model_name != MODEL:
            raise ValueError(f"PiAgent requires model_name={MODEL!r}")
        if version not in (None, _PI_VERSION):
            raise ValueError(f"PiAgent requires version={_PI_VERSION!r}")
        self._wire_binary_path = validate_linux_binary(wire_binary_path)
        super().__init__(
            logs_dir=logs_dir,
            model_name=model_name,
            logger=logger,
            extra_env=extra_env,
            version=_PI_VERSION,
        )

    @staticmethod
    def name() -> str:
        return "pi"

    async def install(self, environment: BaseEnvironment) -> None:
        await self.ensure_system_dependencies(environment, ("curl", "tar"))
        await self.exec_as_root(
            environment,
            command=(
                "set -eu; "
                "mkdir -p /installed-agent/pi-install-home /installed-agent/pi-nvm "
                f"{_LOGS}; "
                "export HOME=/installed-agent/pi-install-home "
                "NVM_DIR=/installed-agent/pi-nvm PROFILE=/dev/null; "
                "curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.2/install.sh "
                "-o /installed-agent/pi-nvm-install.sh; "
                "env -u NODE_VERSION bash /installed-agent/pi-nvm-install.sh; "
                '. "$NVM_DIR/nvm.sh"; '
                f"nvm install {_NODE_VERSION}; "
                f"nvm alias default {_NODE_VERSION}; "
                f"npm install -g --ignore-scripts @earendil-works/pi-coding-agent@{_PI_VERSION}; "
                f"pi --version >{_LOGS}/pi-version.txt; "
                f"node --version >{_LOGS}/pi-node-version.txt; "
                f"npm --version >{_LOGS}/pi-npm-version.txt"
            ),
        )
        await environment.upload_file(
            Path(__file__).with_name("pi-route.ts"), "/installed-agent/pi-route.ts"
        )
        await install_wire(self, environment, self._wire_binary_path)

    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        env = _isolated_env("pi")
        await self.exec_as_agent(
            environment,
            command='umask 077; mkdir -p "$PI_CODING_AGENT_DIR"',
            env=env,
        )
        arguments = [
            f"{_PI_BIN}/pi",
            "--offline", "--no-extensions", "--no-skills", "--no-prompt-templates",
            "--no-themes", "--no-context-files", "--approve", "--no-session",
            "--provider", "inkling-eval", "--model", MODEL,
            "--thinking", "high", "--extension", "/installed-agent/pi-route.ts",
            "--mode", "json", "--print", "--", instruction,
        ]
        await exec_wire(
            self, environment, command=_launch_command(arguments, _PI_BIN), env=env
        )
        # Harbor synchronizes logs before calling populate_context_post_run.

    def populate_context_post_run(self, context: AgentContext) -> None:
        populate_wire_context(self.logs_dir, context)


class OMPAgent(BaseInstalledAgent):
    """Stock OMP 18.6.1 on pinned Bun, including its native subagent loop."""

    def __init__(
        self,
        logs_dir: Path,
        model_name: str,
        *,
        wire_binary_path: str | Path,
        logger: logging.Logger | None = None,
        extra_env: dict[str, str] | None = None,
        version: str | None = None,
    ) -> None:
        if model_name != MODEL:
            raise ValueError(f"OMPAgent requires model_name={MODEL!r}")
        if version not in (None, _OMP_VERSION):
            raise ValueError(f"OMPAgent requires version={_OMP_VERSION!r}")
        self._wire_binary_path = validate_linux_binary(wire_binary_path)
        super().__init__(
            logs_dir=logs_dir,
            model_name=model_name,
            logger=logger,
            extra_env=extra_env,
            version=_OMP_VERSION,
        )

    @staticmethod
    def name() -> str:
        return "omp"

    async def install(self, environment: BaseEnvironment) -> None:
        await self.ensure_system_dependencies(environment, ("curl", "unzip"))
        await self.exec_as_root(
            environment,
            command=(
                "set -eu; "
                f"mkdir -p /installed-agent/omp-install-home {_LOGS}; "
                "export HOME=/installed-agent/omp-install-home "
                "BUN_INSTALL=/installed-agent/omp-bun; "
                "curl -fsSL https://bun.sh/install -o /installed-agent/omp-bun-install.sh; "
                f"bash /installed-agent/omp-bun-install.sh bun-v{_BUN_VERSION}; "
                f'export PATH={_OMP_BIN}:"$PATH"; '
                f"bun install -g @oh-my-pi/pi-coding-agent@{_OMP_VERSION}; "
                f"omp --version >{_LOGS}/omp-version.txt; "
                f"bun --version >{_LOGS}/omp-bun-version.txt"
            ),
        )
        await environment.upload_file(
            Path(__file__).with_name("omp-route.ts"), "/installed-agent/omp-route.ts"
        )
        await install_wire(self, environment, self._wire_binary_path)

    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        env = _isolated_env("omp")
        # Role selectors cover auxiliary native calls, not just task subagents.
        # Leave retry counts, stock tools, prompts, and compaction defaults alone.
        settings = "retry:\n  modelFallback: false\nmodelRoles:\n" + "".join(
            f"  {role}: {_MODEL_SELECTOR}:high\n"
            for role in ("default", "smol", "slow", "plan", "commit", "vision", "task", "advisor", "tiny")
        )
        await self.exec_as_agent(
            environment,
            command=(
                'umask 077; mkdir -p "$PI_CODING_AGENT_DIR" && '
                f"printf %s {shlex.quote(settings)} "
                '>"$PI_CODING_AGENT_DIR/config.yml"'
            ),
            env=env,
        )
        arguments = [
            f"{_OMP_BIN}/omp",
            "--print", "--mode", "json", "--no-session", "--no-extensions",
            "--no-skills", "--no-rules", "--no-title", "--auto-approve",
            "--thinking", "high", "--model", _MODEL_SELECTOR,
            "--smol", _MODEL_SELECTOR, "--slow", _MODEL_SELECTOR,
            "--plan", _MODEL_SELECTOR, "--extension", "/installed-agent/omp-route.ts",
            "--", instruction,
        ]
        await exec_wire(
            self, environment, command=_launch_command(arguments, _OMP_BIN), env=env
        )

    def populate_context_post_run(self, context: AgentContext) -> None:
        populate_wire_context(self.logs_dir, context)
