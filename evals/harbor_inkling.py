"""Run the prebuilt think CLI with the fixed Terminal-Bench profile."""

import logging
import shlex
from pathlib import Path

from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

from evals.harbor_wire import MODEL, exec_wire, install_wire, populate_wire_context, validate_linux_binary


_BINARY = "/installed-agent/think"
_LOGS = "/logs/agent"


class InklingAgent(BaseInstalledAgent):
    def __init__(
        self,
        logs_dir: Path,
        model_name: str,
        *,
        binary_path: str | Path,
        wire_binary_path: str | Path,
        logger: logging.Logger | None = None,
        extra_env: dict[str, str] | None = None,
        version: str | None = None,
    ) -> None:
        if model_name != MODEL:
            raise ValueError(f"InklingAgent requires model_name={MODEL!r}")
        self._binary_path = validate_linux_binary(binary_path)
        self._wire_binary_path = validate_linux_binary(wire_binary_path)
        super().__init__(
            logs_dir=logs_dir,
            model_name=model_name,
            logger=logger,
            extra_env=extra_env,
            version=version,
        )

    @staticmethod
    def name() -> str:
        return "inkling"

    async def install(self, environment: BaseEnvironment) -> None:
        await install_wire(self, environment, self._wire_binary_path)
        await environment.upload_file(self._binary_path, _BINARY)
        await self.exec_as_root(environment, command=f"chmod 755 {_BINARY}")

    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        arguments = [
            _BINARY,
            "run",
            "-model", MODEL,
            "-effort", "high",
            "-max-tokens", "16384",
            "-max-turns", "10000",
            "-yes",
            "-json",
            "-extra", '{"provider":{"order":["deepinfra/fp8"],"allow_fallbacks":false}}',
        ]
        child_command = (
            f"exec {shlex.join(arguments)} "
            '-base-url "${HARBOR_MESSAGES_BASE_URL:?Recorder did not supply Messages base}" -- "$1"'
        )
        child = ["/bin/bash", "-c", child_command, "harbor-inkling", instruction]
        await exec_wire(
            self, environment,
            command=(
                "exec /installed-agent/harbor-wire "
                f"-logs {_LOGS}/wire -messages-policy normalize -- "
                f"{shlex.join(child)} </dev/null "
                f">{_LOGS}/events.jsonl 2>{_LOGS}/stderr.log"
            ),
        )
        # Leave context empty so Harbor reads records after log sync, also on failure.

    def populate_context_post_run(self, context: AgentContext) -> None:
        populate_wire_context(self.logs_dir, context)
