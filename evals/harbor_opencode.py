"""Run native OpenCode 1.18.35 through the fixed Messages recorder.

The npm postinstall script copies or links the platform binary to bin/opencode.exe.
Run that file, not the old Node shim. Keep native prompts, tools, and compaction.
Harbor controls the task timeout. The recorder controls the profile and accounting.

OpenCode has no public control to turn off all request retries at this pin.
The session loop permits five retries. Title generation sets two SDK retries;
other session calls use zero SDK retries. This adapter adds no retries.

Pinned source:
https://registry.npmjs.org/opencode-ai/1.18.35
https://github.com/anomalyco/opencode/blob/v1.18.35/packages/opencode/script/postinstall.mjs
https://github.com/anomalyco/opencode/blob/v1.18.35/packages/opencode/src/config/config.ts
https://github.com/anomalyco/opencode/blob/v1.18.35/packages/opencode/src/session/retry.ts
https://github.com/anomalyco/opencode/blob/v1.18.35/packages/opencode/src/session/prompt.ts
https://github.com/anomalyco/opencode/blob/v1.18.35/packages/opencode/src/session/llm.ts
"""

import json
import logging
import shlex
from pathlib import Path

from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.agents.installed.node_install import nvm_node_install_snippet
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
_VERSION = "1.18.35"
_ROOT = "/installed-agent/opencode"
_HOME = f"{_ROOT}/home"
_BIN = f"{_ROOT}/bin"
_PREFIX = f"{_ROOT}/npm"
_EXECUTABLE = f"{_PREFIX}/lib/node_modules/opencode-ai/bin/opencode.exe"
_MODEL_SELECTOR = f"anthropic/{MODEL}"


def _isolated_env() -> dict[str, str]:
    return {
        "HOME": _HOME,
        "XDG_CONFIG_HOME": f"{_HOME}/.config",
        "XDG_DATA_HOME": f"{_HOME}/.local/share",
        "XDG_STATE_HOME": f"{_HOME}/.local/state",
        "XDG_CACHE_HOME": f"{_HOME}/.cache",
        "XDG_RUNTIME_DIR": f"{_HOME}/.run",
        "TMPDIR": f"{_HOME}/tmp",
        "NVM_DIR": f"{_HOME}/.nvm",
        "NVM_NODEJS_ORG_MIRROR": "https://nodejs.org/dist",
        "PROFILE": "/dev/null",
        "SHELL": "/bin/bash",
        "OPENCODE_CONFIG_DIR": f"{_HOME}/.config/opencode",
    }


def _config_content() -> str:
    # Inline config takes priority over project config and agent files. It does
    # not disable project instructions. Managed config can still take priority.
    return json.dumps(
        {
            "$schema": "https://opencode.ai/config.json",
            "autoupdate": False,
            "model": _MODEL_SELECTOR,
            "small_model": _MODEL_SELECTOR,
            "enabled_providers": ["anthropic"],
            "agent": {
                name: {"model": _MODEL_SELECTOR, "variant": "high"}
                for name in ("build", "plan", "general", "explore", "compaction", "title", "summary")
            },
            "provider": {
                "anthropic": {
                    "npm": "@ai-sdk/anthropic",
                    # Reject an unpinned model in project-defined agent slots.
                    "whitelist": [MODEL],
                    "options": {
                        "baseURL": "{env:HARBOR_MESSAGES_BASE_URL}/v1",
                        "apiKey": "{env:HARBOR_WIRE_TOKEN}",
                    },
                    "models": {
                        MODEL: {
                            "name": "Inkling Small",
                            "reasoning": True,
                            "tool_call": True,
                            "limit": {"context": 524288, "output": 16384},
                        }
                    },
                }
            },
        },
        separators=(",", ":"),
    )


class OpenCodeAgent(BaseInstalledAgent):
    """Use the pinned native OpenCode CLI and recorder-owned token counts."""

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
            raise ValueError(f"OpenCodeAgent requires model_name={MODEL!r}")
        if version not in (None, _VERSION):
            raise ValueError(f"OpenCodeAgent requires version={_VERSION!r}")
        self._wire_binary_path = validate_linux_binary(wire_binary_path)
        super().__init__(
            logs_dir=logs_dir,
            model_name=model_name,
            logger=logger,
            extra_env=extra_env,
            version=_VERSION,
        )

    @staticmethod
    def name() -> str:
        return "opencode"

    async def install(self, environment: BaseEnvironment) -> None:
        await self.ensure_system_dependencies(environment, ("curl", "bash", "ripgrep"))
        await self.exec_as_root(environment, command=f"mkdir -p {_ROOT} {_LOGS}")
        if environment.default_user is not None:
            owner = shlex.quote(str(environment.default_user))
            await self.exec_as_root(environment, command=f"chown {owner} {_ROOT} {_LOGS}")
        await self.exec_as_agent(
            environment,
            command=(
                f"umask 077; unset OPENCODE_CONFIG; mkdir -p {_BIN} {_PREFIX} {_HOME}/.config/opencode "
                f"{_HOME}/.local/share {_HOME}/.local/state {_HOME}/.cache "
                f"{_HOME}/.run {_HOME}/tmp {_HOME}/.nvm && "
                + nvm_node_install_snippet()
                + f" && npm install --prefix {_PREFIX} -g opencode-ai@{_VERSION}"
                + f' && ln -sf "$(command -v node)" {_BIN}/node'
                + f' && version="$({_EXECUTABLE} --version)"'
                + f' && test "$version" = {shlex.quote(_VERSION)}'
                + ' && { printf "opencode %s\\n" "$version"; '
                + "node --version; npm --version; uname -srm; } "
                + f">{_LOGS}/native-versions.txt"
            ),
            env=_isolated_env(),
        )
        await install_wire(self, environment, self._wire_binary_path)

    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        arguments = [
            _EXECUTABLE,
            "run",
            "--model", _MODEL_SELECTOR,
            "--variant", "high",
            "--format", "json",
            "--thinking",
            "--auto",
            "--", instruction,
        ]
        # OpenCode expands the env placeholders after the recorder sets them.
        # The recorder starts the native binary as its direct child.
        await exec_wire(
            self,
            environment,
            command=(
                f'unset OPENCODE_CONFIG; export PATH={_BIN}:{_PREFIX}/bin:"$PATH"; '
                "exec /installed-agent/harbor-wire "
                f"-logs {_LOGS}/wire -messages-policy normalize -- "
                f"{shlex.join(arguments)} </dev/null "
                f">{_LOGS}/events.jsonl 2>{_LOGS}/stderr.log"
            ),
            env={**_isolated_env(), "OPENCODE_CONFIG_CONTENT": _config_content()},
        )
        # Harbor syncs native logs before it calls the context hook.

    def populate_context_post_run(self, context: AgentContext) -> None:
        populate_wire_context(self.logs_dir, context)
