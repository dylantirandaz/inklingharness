"""Run native Hermes Agent 0.19.0 through the fixed Messages recorder.

The PyPI wheel installs in a uv-managed CPython 3.12.13 virtual environment.
The anthropic extra adds the SDK for Hermes' native anthropic_messages
transport. Dependency resolution excludes uploads after the 0.19.0 release.

Hermes runs as ``hermes -z`` (one-shot). That mode sets HERMES_YOLO_MODE and
HERMES_ACCEPT_HOOKS, answers clarify questions itself, and runs delegate_task
inline. It keeps the native system prompt, the default hermes-cli toolsets,
context files, memory, compression, skills, and the 90-turn limit. HERMES_HOME
is new for each trial. Tool subprocesses keep the task HOME, as native Hermes
does. Harbor owns the task timeout.

The model comes from a user-defined provider named harbor-wire. It uses the
anthropic_messages transport, so Hermes keeps the model name unchanged. In -z
mode Hermes sends no reasoning settings. The recorder sets the profile's
effort, output cap, and route policy. The Responses transport is not
used. Without a reasoning setting it sends medium effort, and the recorder
rejects that. Auxiliary tasks such as compression and titles default to the
main provider and model.

Hermes probes loopback endpoints for local servers and /models pricing data.
The recorder rejects those paths and stops the run. hermes_wire_filter.py
forwards only recordable requests and refuses the rest locally.

Hermes makes its own request retries (agent.api_max_retries defaults to 3).
The Anthropic SDK retries are set to zero. This adapter adds no retries.

Pinned source: https://pypi.org/project/hermes-agent/0.19.0/
(sha256 bd0bac012aee38a60894781f4597dc29ee7bedb3448540249921f10d3bef327f).
Headless mode: hermes_cli/oneshot.py. Provider resolution:
hermes_cli/runtime_provider.py (_get_named_custom_provider). Probes:
agent/model_metadata.py (detect_local_server_type, fetch_endpoint_model_metadata).
"""

import logging
import shlex
from pathlib import Path

from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

from evals.harbor_wire import (
    PROMPT_EXTRAS,
    with_task_extras,
    MODEL,
    WireProfile,
    exec_wire,
    install_wire,
    populate_wire_context,
    profile_for,
    validate_linux_binary,
)

_VERSION = "0.19.0"
_PACKAGE = f"hermes-agent[anthropic]=={_VERSION}"
_EXCLUDE_NEWER = "2026-07-21T00:00:00Z"
_PYTHON = "3.12.13"
_UV_VERSION = "0.11.7"
_UV_ARCHIVE = "uv-x86_64-unknown-linux-musl.tar.gz"
_UV_SHA256 = "64ddb5f1087649e3f75aa50d139aa4f36ddde728a5295a141e0fa9697bfb7b0f"
_LOGS = "/logs/agent"
_ROOT = "/installed-agent/hermes"
_VENV = f"{_ROOT}/venv"
_PYTHON_BIN = f"{_VENV}/bin/python"
_EXECUTABLE = f"{_VENV}/bin/hermes"
_FILTER = f"{_ROOT}/hermes_wire_filter.py"
_FILTER_SOURCE = Path(__file__).with_name("hermes_wire_filter.py")
_HERMES_HOME = f"{_LOGS}/hermes-home"
_PROVIDER = "harbor-wire"


def _uv_env() -> dict[str, str]:
    return {
        "UV_NO_CONFIG": "1",
        "UV_CACHE_DIR": f"{_ROOT}/uv-cache",
        "UV_PYTHON_INSTALL_DIR": f"{_ROOT}/python",
        "UV_PYTHON_DOWNLOADS": "automatic",
        "UV_MANAGED_PYTHON": "1",
    }


def install_command() -> str:
    uv = f"{_ROOT}/bin/uv"
    return (
        "set -eu; umask 077; "
        f"mkdir -p {_ROOT}/bin {_LOGS}; "
        "curl -fsSL "
        f"https://github.com/astral-sh/uv/releases/download/{_UV_VERSION}/{_UV_ARCHIVE} "
        f"-o {_ROOT}/{_UV_ARCHIVE}; "
        f"printf '%s  %s\\n' {_UV_SHA256} {_ROOT}/{_UV_ARCHIVE} | sha256sum -c - >/dev/null; "
        f"tar -xzf {_ROOT}/{_UV_ARCHIVE} -C {_ROOT}/bin --strip-components 1; "
        f"{uv} venv --quiet --python {_PYTHON} {_VENV}; "
        f"{uv} pip install --quiet --python {_PYTHON_BIN} "
        f"--exclude-newer {_EXCLUDE_NEWER} {shlex.quote(_PACKAGE)}; "
        f"version=$({_PYTHON_BIN} -c "
        + shlex.quote("import importlib.metadata as m; print(m.version('hermes-agent'))")
        + "); "
        f'test "$version" = {shlex.quote(_VERSION)}; '
        f"{{ HERMES_HOME={_ROOT}/version-home {_EXECUTABLE} --version; "
        f"{uv} --version; uname -srm; }} >{_LOGS}/native-versions.txt"
    )


def config_content(profile: WireProfile | None = None) -> str:
    # Hermes expands ${VAR} when it loads config, inside the recorder child.
    profile = profile or profile_for(MODEL)
    return (
        "model:\n"
        f"  provider: {_PROVIDER}\n"
        f"  default: {profile.model}\n"
        f"  context_length: {profile.context_length}\n"
        f"  max_tokens: {profile.max_tokens}\n"
        "providers:\n"
        f"  {_PROVIDER}:\n"
        f"    name: {_PROVIDER}\n"
        "    base_url: ${HERMES_WIRE_BASE_URL}\n"
        "    key_env: HARBOR_WIRE_TOKEN\n"
        "    api_mode: anthropic_messages\n"
        f"    default_model: {profile.model}\n"
    )


def child_command(profile: WireProfile | None = None) -> str:
    """Start the filter, then exec Hermes so the recorder samples Hermes itself."""
    profile = profile or profile_for(MODEL)
    port_file = f"{_LOGS}/hermes-filter.port"
    hermes = [
        _EXECUTABLE,
        "-z", '"$1"',
        "--provider", _PROVIDER,
        "-m", profile.model,
        "--usage-file", f"{_LOGS}/hermes-usage.json",
    ]
    return (
        "set -eu; "
        "unset HERMES_MANAGED_DIR HERMES_CONFIG HERMES_ENV HERMES_PROFILE "
        "HERMES_INFERENCE_MODEL HERMES_INFERENCE_PROVIDER; "
        f"rm -f {port_file}; "
        f"{_PYTHON_BIN} {_FILTER} "
        '"${HARBOR_MESSAGES_BASE_URL:?Recorder did not supply Messages base}" '
        f"{port_file} {_LOGS}/hermes-filter.jsonl & "
        "filter=$!; "
        f"for _ in $(seq 100); do test -s {port_file} && break; "
        'kill -0 "$filter"; sleep 0.1; done; '
        f"read -r port <{port_file}; "
        'export HERMES_WIRE_BASE_URL="http://127.0.0.1:$port/api"; '
        # The prompt is a positional argument, never shell source.
        "exec " + " ".join(hermes)
    )


class HermesAgent(BaseInstalledAgent):
    capabilities = PROMPT_EXTRAS

    """Stock Hermes Agent one-shot mode with recorder-owned token counts."""

    def __init__(
        self,
        logs_dir: Path,
        model_name: str,
        *,
        wire_binary_path: str | Path,
        logger: logging.Logger | None = None,
        skills_dir: str | None = None,
        mcp_servers: list | None = None,
        extra_env: dict[str, str] | None = None,
        version: str | None = None,
        effort: str | None = None,
    ) -> None:
        self._profile = profile_for(model_name, effort)
        if version not in (None, _VERSION):
            raise ValueError(f"HermesAgent requires version={_VERSION!r}")
        self._wire_binary_path = validate_linux_binary(wire_binary_path)
        super().__init__(
            logs_dir=logs_dir,
            model_name=model_name,
            logger=logger,
            skills_dir=skills_dir,
            mcp_servers=mcp_servers,
            extra_env=extra_env,
            version=_VERSION,
        )

    @staticmethod
    def name() -> str:
        return "hermes"

    async def install(self, environment: BaseEnvironment) -> None:
        await self.ensure_system_dependencies(environment, ("curl", "bash", "tar", "coreutils", "ripgrep"))
        await self.exec_as_root(environment, command=f"mkdir -p {_ROOT} {_LOGS}")
        if environment.default_user is not None:
            owner = shlex.quote(str(environment.default_user))
            await self.exec_as_root(environment, command=f"chown {owner} {_ROOT} {_LOGS}")
        await self.exec_as_agent(environment, command=install_command(), env=_uv_env())
        await self._upload_config_text(
            environment,
            content=_FILTER_SOURCE.read_text(),
            remote_path=_FILTER,
            filename=_FILTER_SOURCE.name,
        )
        await install_wire(self, environment, self._wire_binary_path)

    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        instruction = with_task_extras(self, instruction)
        env = {"HERMES_HOME": _HERMES_HOME}
        await self.exec_as_agent(
            environment,
            command=(
                "set -eu; umask 077; "
                "if test -e /etc/hermes; then "
                "printf '%s\\n' 'Hermes requires an isolated managed scope' >&2; "
                "exit 1; fi; "
                'mkdir -p "$HERMES_HOME"; '
                f"printf %s {shlex.quote(config_content(self._profile))} "
                '>"$HERMES_HOME/config.yaml"'
            ),
            env=env,
        )
        arguments = ["/bin/bash", "-c", child_command(self._profile), "hermes-wire", instruction]
        # -z prints only the final response; the session store, agent.log,
        # and usage report stay under /logs/agent for Harbor to sync.
        await exec_wire(
            self,
            environment,
            command=(
                "exec /installed-agent/harbor-wire "
                f"-logs {_LOGS}/wire -messages-policy normalize {shlex.join(self._profile.wire_flags())} -- "
                f"{shlex.join(arguments)} </dev/null "
                f">{_LOGS}/final-response.txt 2>{_LOGS}/stderr.log"
            ),
            env=env,
        )
        # Harbor synchronizes native logs before it calls this hook.

    def populate_context_post_run(self, context: AgentContext) -> None:
        populate_wire_context(self.logs_dir, context)
