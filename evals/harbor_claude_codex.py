"""Stock Claude/Codex peers with isolated config and recorder-owned accounting.

Installs use the SDK's Node 22 installer (glibc Linux) and pinned npm packages.
Both peers retain native prompts, tools, compaction, and zero request retries;
Harbor, not these adapters, owns the official task timeout. Runtime and CLI
versions are captured at install time, without logging credential values.
"""

import json
import logging
import shlex
from pathlib import Path
from typing import ClassVar

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
_ROOT = "/installed-agent/native-peer"
_HOME = f"{_ROOT}/home"
_BIN = f"{_ROOT}/bin"
_PREFIX = f"{_ROOT}/npm"


class _NativePeer(BaseInstalledAgent):
    PINNED_VERSION: ClassVar[str]
    PACKAGE: ClassVar[str]
    EXECUTABLE: ClassVar[str]

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
            raise ValueError(f"This comparison requires model {MODEL!r}")
        if version is not None and version != self.PINNED_VERSION:
            raise ValueError(f"This peer requires version {self.PINNED_VERSION!r}")
        self._wire_binary = validate_linux_binary(wire_binary_path)
        super().__init__(
            logs_dir=logs_dir,
            model_name=model_name,
            logger=logger,
            extra_env=extra_env,
            version=self.PINNED_VERSION,
        )

    async def install(self, environment: BaseEnvironment) -> None:
        await self.ensure_system_dependencies(environment, ("curl", "bash", "ripgrep"))
        await self.exec_as_root(
            environment,
            command=f"mkdir -p {_ROOT} {_LOGS}",
        )
        if environment.default_user is not None:
            owner = shlex.quote(str(environment.default_user))
            await self.exec_as_root(
                environment,
                command=f"chown {owner} {_ROOT} {_LOGS}",
            )
        await self.exec_as_agent(
            environment,
            command=(
                f"mkdir -p {_BIN} {_PREFIX} {_HOME}/.config "
                f"{_HOME}/.local/state {_HOME}/.cache {_HOME}/claude {_HOME}/codex && "
                + nvm_node_install_snippet()
                + f" && npm install --prefix {_PREFIX} -g "
                + shlex.quote(f"{self.PACKAGE}@{self.PINNED_VERSION}")
                + f' && ln -sf "$(command -v node)" {_BIN}/node'
                + f" && {{ {_PREFIX}/bin/{self.EXECUTABLE} --version && "
                + "node --version && npm --version && uname -srm; } "
                + f">{_LOGS}/native-versions.txt"
            ),
            env={"HOME": _HOME, "NVM_DIR": f"{_HOME}/.nvm", "NVM_NODEJS_ORG_MIRROR": "https://nodejs.org/dist"},
        )
        await install_wire(self, environment, self._wire_binary)

    async def _launch(
        self,
        environment: BaseEnvironment,
        arguments: list[str],
        env: dict[str, str],
    ) -> None:
        await exec_wire(
            self, environment,
            command=(
                f'export PATH={_BIN}:{_PREFIX}/bin:"$PATH"; '
                "exec /installed-agent/harbor-wire "
                f"-logs {_LOGS}/wire -messages-policy verify -- "
                f"{shlex.join(arguments)} </dev/null "
                f">{_LOGS}/events.jsonl 2>{_LOGS}/stderr.log"
            ),
            env={"SHELL": "/bin/bash", **env},
        )

    def populate_context_post_run(self, context: AgentContext) -> None:
        populate_wire_context(self.logs_dir, context)


class ClaudeCodeAgent(_NativePeer):
    PINNED_VERSION = "2.1.205"
    PACKAGE = "@anthropic-ai/claude-code"
    EXECUTABLE = "claude"

    @staticmethod
    def name() -> str:
        return "claude-code"

    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        env = {
            "CLAUDE_CONFIG_DIR": f"{_HOME}/claude",
            "ANTHROPIC_MODEL": MODEL,
            "ANTHROPIC_SMALL_FAST_MODEL": MODEL,
            "CLAUDE_CODE_SUBAGENT_MODEL": MODEL,
            "ANTHROPIC_DEFAULT_FABLE_MODEL": MODEL,
            "ANTHROPIC_DEFAULT_OPUS_MODEL": MODEL,
            "ANTHROPIC_DEFAULT_SONNET_MODEL": MODEL,
            "ANTHROPIC_DEFAULT_HAIKU_MODEL": MODEL,
            "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "16384",
            "CLAUDE_CODE_MAX_RETRIES": "0",
            "CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK": "1",
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
            "CLAUDE_CODE_AUTO_CONNECT_IDE": "false",
            "IS_SANDBOX": "1",
            "CLAUDE_CODE_EXTRA_BODY": json.dumps(
                {
                    "provider": {"order": ["deepinfra/fp8"], "allow_fallbacks": False},
                    "output_config": {"effort": "high"},
                    "max_tokens": 16384,
                },
                separators=(",", ":"),
            ),
        }
        # The recorder supplies the dynamic Messages base and loopback auth.
        await self._launch(
            environment,
            [
                f"{_PREFIX}/bin/claude",
                "--safe-mode",
                "--print",
                "--verbose",
                "--output-format", "stream-json",
                "--include-partial-messages",
                "--no-session-persistence",
                "--dangerously-skip-permissions",
                "--setting-sources", "",
                "--strict-mcp-config",
                "--model", MODEL,
                "--effort", "high",
                "--", instruction,
            ],
            env,
        )


class CodexAgent(_NativePeer):
    PINNED_VERSION = "0.146.0"
    PACKAGE = "@openai/codex"
    EXECUTABLE = "codex"

    @staticmethod
    def name() -> str:
        return "codex"

    async def install(self, environment: BaseEnvironment) -> None:
        await super().install(environment)
        package_root = f"{_PREFIX}/lib/node_modules/@openai/codex"
        locate = (
            "const path = require('node:path'); "
            f"const resolve = require('node:module').createRequire({json.dumps(package_root + '/package.json')}); "
            "const root = path.dirname(resolve.resolve('@openai/codex-linux-x64/package.json')); "
            "process.stdout.write(path.join(root, 'vendor/x86_64-unknown-linux-musl/bin/codex'));"
        )
        await self.exec_as_agent(
            environment,
            command=f'ln -s "$({_BIN}/node -e {shlex.quote(locate)})" {_BIN}/codex-native',
        )

    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        arguments = [
            f"{_BIN}/codex-native", "exec",
            "--ignore-user-config",
            "--ignore-rules",
            "--ephemeral",
            "--skip-git-repo-check",
            "--dangerously-bypass-approvals-and-sandbox",
            "--json",
        ]
        for setting in (
            f'model="{MODEL}"',
            'model_provider="canary_openrouter"',
            'model_reasoning_effort="high"',
            'model_providers.canary_openrouter.name="Private OpenRouter Responses canary"',
            'model_providers.canary_openrouter.env_key="OPENROUTER_API_KEY"',
            'model_providers.canary_openrouter.wire_api="responses"',
            'model_providers.canary_openrouter.supports_websockets=false',
            'model_providers.canary_openrouter.request_max_retries=0',
            'model_providers.canary_openrouter.stream_max_retries=0',
        ):
            arguments.extend(["-c", setting])
        # Expand only in the recorder's child. The instruction is positional,
        # never shell source; the loopback URL remains one quoted TOML argument.
        child_command = (
            f"exec {shlex.join(arguments)} "
            '-c "model_providers.canary_openrouter.base_url=\\"'
            '${HARBOR_RESPONSES_BASE_URL:?Recorder did not supply Responses base}'
            '\\"" -- "$1"'
        )
        await self._launch(
            environment,
            ["/bin/bash", "-c", child_command, "harbor-codex", instruction],
            {
                "CODEX_HOME": f"{_HOME}/codex",
                "CODEX_MANAGED_BY_NPM": "1",
                "CODEX_MANAGED_PACKAGE_ROOT": f"{_PREFIX}/lib/node_modules/@openai/codex",
            },
        )
