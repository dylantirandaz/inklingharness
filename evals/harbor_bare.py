"""Run the no-harness baseline: the model with one bash tool in a bare loop.

The binary is built from evals/bare. It sends no system prompt and keeps no
planning, context, or recovery policy, so the comparison shows what each
harness adds to the model. It runs under harbor-wire with the same fixed wire
profile as every other native peer.
Without an explicit source version, identify the binary by its SHA-256 digest.
"""

import hashlib
import logging
import shlex
from pathlib import Path

from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

from evals.harbor_wire import PROMPT_EXTRAS, exec_wire, install_wire, populate_wire_context, profile_for, validate_linux_binary, with_task_extras


_BINARY = "/installed-agent/bare"
_LOGS = "/logs/agent"


class BareAgent(BaseInstalledAgent):
    capabilities = PROMPT_EXTRAS

    def __init__(
        self,
        logs_dir: Path,
        model_name: str,
        *,
        binary_path: str | Path,
        wire_binary_path: str | Path,
        logger: logging.Logger | None = None,
        skills_dir: str | None = None,
        mcp_servers: list | None = None,
        extra_env: dict[str, str] | None = None,
        version: str | None = None,
        effort: str | None = None,
    ) -> None:
        self._profile = profile_for(model_name, effort)
        self._binary_path = validate_linux_binary(binary_path)
        self._wire_binary_path = validate_linux_binary(wire_binary_path)
        if version is None:
            with self._binary_path.open("rb") as binary:
                version = "sha256:" + hashlib.file_digest(binary, "sha256").hexdigest()
        super().__init__(
            logs_dir=logs_dir,
            model_name=model_name,
            logger=logger,
            skills_dir=skills_dir,
            mcp_servers=mcp_servers,
            extra_env=extra_env,
            version=version,
        )

    @staticmethod
    def name() -> str:
        return "bare"

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
        instruction = with_task_extras(self, instruction)
        arguments = [
            _BINARY,
            "-model", self._profile.model,
            "-max-tokens", str(self._profile.max_tokens),
            "-max-turns", "10000",
        ]
        child_command = (
            f"exec {shlex.join(arguments)} "
            '-base-url "${HARBOR_MESSAGES_BASE_URL:?Recorder did not supply Messages base}" -- "$1"'
        )
        child = ["/bin/bash", "-c", child_command, "harbor-bare", instruction]
        await exec_wire(
            self, environment,
            command=(
                "exec /installed-agent/harbor-wire "
                f"-logs {_LOGS}/wire -messages-policy normalize {shlex.join(self._profile.wire_flags())} -- "
                f"{shlex.join(child)} </dev/null "
                f">{_LOGS}/events.jsonl 2>{_LOGS}/stderr.log"
            ),
        )
        # Leave context empty so Harbor reads records after log sync, also on failure.

    def populate_context_post_run(self, context: AgentContext) -> None:
        populate_wire_context(self.logs_dir, context)
