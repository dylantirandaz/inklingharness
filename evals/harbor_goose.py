"""Run native Goose v1.54.0 through the fixed Messages recorder.

The tagged installer and its GOOSE_VERSION select the same release. Goose keeps
its native prompts, tools, compaction, and request retries. The standard Linux
build enables developer, analyze, apps, extensionmanager, scheduler, summon,
tom, and skills by default. No external MCP service or recipe is added here.

Titles and compaction use the main model. The subagent environment selects the
same model, but native recipe settings can take precedence over that environment.
The recorder owns the fixed backend, high effort, output cap, and route policy.
Harbor owns the task timeout.

Pinned source: https://github.com/aaif-goose/goose/tree/v1.54.0
Model selection: crates/goose/src/model_config.rs and
crates/goose/src/agents/platform_extensions/summon.rs.
Retries: crates/goose-provider-types/src/retry.rs and
crates/goose-providers/src/anthropic.rs. Anthropic has no CLI or environment
control for its three request retries or its thinking-signature repair retry.
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


_VERSION = "v1.54.0"
_PREFIX = "/installed-agent/goose"
_BINARY = f"{_PREFIX}/bin/goose"
_LOGS = "/logs/agent"
_HOME = f"{_LOGS}/goose-home"


def _isolated_env(home: str) -> dict[str, str]:
    return {
        "HOME": home,
        "XDG_CONFIG_HOME": f"{home}/.config",
        "XDG_DATA_HOME": f"{home}/.local/share",
        "XDG_CACHE_HOME": f"{home}/.cache",
        "XDG_STATE_HOME": f"{home}/.local/state",
        "XDG_RUNTIME_DIR": f"{home}/run",
        "GOOSE_PATH_ROOT": f"{home}/goose",
        "GOOSE_DISABLE_KEYRING": "1",
    }


class GooseAgent(BaseInstalledAgent):
    """Stock Goose with isolated settings and native default extensions."""

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
            raise ValueError(f"GooseAgent requires model_name={MODEL!r}")
        if version not in (None, _VERSION):
            raise ValueError(f"GooseAgent requires version={_VERSION!r}")
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
        return "goose"

    async def install(self, environment: BaseEnvironment) -> None:
        await self.ensure_system_dependencies(
            environment, ("curl", "tar", "bzip2", "libxcb", "libgomp")
        )
        env = _isolated_env(f"{_PREFIX}/install-home")
        env.update({
            "GOOSE_VERSION": _VERSION,
            "GOOSE_BIN_DIR": f"{_PREFIX}/bin",
            "GOOSE_LINUX_VARIANT": "standard",
            "CONFIGURE": "false",
        })
        await self.exec_as_root(
            environment,
            command=f"mkdir -p {_PREFIX}/bin {_PREFIX}/install-home {_LOGS}",
        )
        await self.exec_as_root(
            environment,
            command=(
                "set -eu; "
                f'export PATH={_PREFIX}/bin:"$PATH"; '
                "curl -fsSL "
                f"https://github.com/aaif-goose/goose/releases/download/{_VERSION}/download_cli.sh "
                f"-o {_PREFIX}/download_cli.sh; "
                f"bash {_PREFIX}/download_cli.sh; "
                f'reported_version=$({_BINARY} --version); '
                f'printf "%s\\n" "$reported_version" >{_LOGS}/goose-version.txt; '
                f'test "${{reported_version##* }}" = {shlex.quote(_VERSION.removeprefix("v"))}'
            ),
            env=env,
            cwd=_PREFIX,
        )
        await install_wire(self, environment, self._wire_binary_path)

    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        env = _isolated_env(_HOME)
        env.update({
            "GOOSE_PROVIDER": "anthropic",
            "GOOSE_MODEL": MODEL,
            "GOOSE_SUBAGENT_PROVIDER": "anthropic",
            "GOOSE_SUBAGENT_MODEL": MODEL,
            "GOOSE_MAX_TOKENS": "16384",
            "GOOSE_CONTEXT_LIMIT": "524288",
            "GOOSE_MODE": "auto",
        })
        # Do not write an extension list: fresh native defaults remain in force.
        settings = f"GOOSE_PROVIDER: anthropic\nGOOSE_MODEL: {MODEL}\n"
        await self.exec_as_agent(
            environment,
            command=(
                "set -eu; umask 077; "
                "if test -e /etc/goose/config.yaml; then "
                "printf '%s\\n' 'Goose requires an isolated system configuration' >&2; "
                "exit 1; fi; "
                'mkdir -p "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$XDG_CACHE_HOME" '
                '"$XDG_STATE_HOME" "$XDG_RUNTIME_DIR" "$GOOSE_PATH_ROOT/config" '
                '"$GOOSE_PATH_ROOT/data" "$GOOSE_PATH_ROOT/state"; '
                f"printf %s {shlex.quote(settings)} "
                '>"$GOOSE_PATH_ROOT/config/config.yaml"'
            ),
            env=env,
        )
        # Expand loopback credentials only in the recorder child, then replace
        # Bash with the Rust executable so the recorder measures Goose itself.
        child = (
            "set -eu; unset GOOSE_ADDITIONAL_CONFIG_FILES; "
            'export ANTHROPIC_HOST="${HARBOR_MESSAGES_BASE_URL:?}" '
            'ANTHROPIC_API_KEY="${HARBOR_WIRE_TOKEN:?}"; '
            f"exec {_BINARY} run --no-session --with-builtin developer "
            '--output-format stream-json -t "$1"'
        )
        arguments = ["/bin/bash", "-c", child, "goose-wire", instruction]
        await exec_wire(
            self,
            environment,
            command=(
                f'export PATH={_PREFIX}/bin:"$PATH"; '
                "exec /installed-agent/harbor-wire "
                f"-logs {_LOGS}/wire -messages-policy normalize -- "
                f"{shlex.join(arguments)} </dev/null "
                f">{_LOGS}/events.jsonl 2>{_LOGS}/stderr.log"
            ),
            env=env,
        )
        # Harbor synchronizes native logs before it calls this hook.

    def populate_context_post_run(self, context: AgentContext) -> None:
        populate_wire_context(self.logs_dir, context)
