"""Think as a Harbor installed agent, set up like the built-in harnesses.

Like the built-ins, Think takes its provider key from the model connection and
its settings from agent options, and runs one CLI command for each task. Its
requests go straight to OpenRouter. No recorder sits between Think and the
provider.

Two things differ from a downloaded harness. The binary comes from a local
Linux build, so a run names the exact build by its SHA-256 digest. The turn
limit is raised: Think stops after 50 turns by default, but the built-in
harnesses have no turn limit, so the task timeout of Harbor is the only bound.
"""

import json
import shlex
from typing import Annotated, Literal, override

from pydantic import Field

from harbor.agents.capabilities import AgentCapabilities
from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.agents.model_connection import ModelConnectionSpec
from harbor.agents.options import Cli, InstalledAgentOptions
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

from evals.harbor_binary import LocalFile, put_file

_REMOTE_BINARY = "/installed-agent/think"
_REMOTE_SYSTEM = "/installed-agent/system.txt"
_EVENTS_FILENAME = "think-events.jsonl"
_STDERR_FILENAME = "think-stderr.log"
_PROVIDER = "openrouter"


class ThinkOptions(InstalledAgentOptions):
    binary_path: str = Field(description="Path of a linux/amd64 build of think on this computer.")
    staged_dir: str | None = Field(
        default=None,
        description="Folder in the sandbox, on a read-only volume, that holds this build under its staged name; see evals.harbor_binary. Without it the build is uploaded.",
    )
    system_file: str | None = Field(
        default=None,
        description="Path on this computer of a system prompt file that replaces the built-in coding guidance (think -system). The file is uploaded with a hash check, and its hash is part of the agent version.",
    )
    reasoning_effort: Annotated[
        Literal["low", "medium", "high", "xhigh", "max"] | None,
        Cli("-effort"),
    ] = Field(default=None, description="Model reasoning effort.")
    max_tokens: Annotated[int | None, Cli("-max-tokens")] = Field(
        default=None, gt=0, description="Output token limit of each model reply."
    )
    max_turns: Annotated[int, Cli("-max-turns")] = Field(
        default=10000, gt=0, description="Most model turns in one run."
    )
    no_web: Annotated[bool, Cli("-no-web")] = Field(
        default=False, description="Do not offer web_search and web_fetch, and leave them out of the prompt."
    )
    lean: Annotated[bool, Cli("-lean")] = Field(
        default=False, description="Headless profile for Inkling Small: read_files, shorter tool output, fewer tools, and the matching prompt (think run -lean)."
    )


class Think(BaseInstalledAgent):
    capabilities = AgentCapabilities()
    MODEL_CONNECTION = ModelConnectionSpec(default_provider=_PROVIDER)
    options_model = ThinkOptions
    options: ThinkOptions

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._local = LocalFile.read(self.options.binary_path)
        with self._local.path.open("rb") as binary:
            header = binary.read(20)
        if header[:6] != b"\x7fELF\x02\x01" or header[18:20] != b"\x3e\x00":
            raise ValueError(f"Expected a Linux amd64 ELF binary: {self._local.path}")
        self._system = LocalFile.read(self.options.system_file) if self.options.system_file else None
        if self._version is None:
            self._version = "sha256:" + self._local.sha256 + (f"+system:{self._system.sha256[:12]}" if self._system else "")

    @staticmethod
    @override
    def name() -> str:
        return "think"

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        # Think finds the project through git, and many task images lack it.
        await self.ensure_system_dependencies(environment, ("git",))
        await put_file(environment, self._local, _REMOTE_BINARY, mode="755", staged_dir=self.options.staged_dir)
        if self._system is not None:
            await put_file(environment, self._system, _REMOTE_SYSTEM, mode="644")

    def _model_id(self) -> str:
        if not self.model_name:
            raise ValueError("Think needs a model name")
        provider, _, model = self.model_name.partition("/")
        if provider == _PROVIDER and model:
            return model
        return self.model_name

    @override
    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        access = self.model_connection
        if access.provider != _PROVIDER or not access.api_key:
            raise ValueError("Think needs an OpenRouter model and OPENROUTER_API_KEY")
        env = {"OPENROUTER_API_KEY": access.api_key}
        command = [_REMOTE_BINARY, "run", "-model", self._model_id(), "-yes", "-json"]
        if self._system is not None:
            command += ["-system", _REMOTE_SYSTEM]
        if self.build_cli_flags():
            command.append(self.build_cli_flags())
        if access.configured_base_url:
            # Think appends /v1/messages to the API root, so it wants the root without /v1.
            command += ["-base-url", access.configured_base_url.removesuffix("/v1")]
        events = (self.environment_logs_dir / _EVENTS_FILENAME).as_posix()
        errors = (self.environment_logs_dir / _STDERR_FILENAME).as_posix()
        await self.exec_as_agent(
            environment,
            command=(
                f"{' '.join(command)} -- {shlex.quote(instruction)} "
                f"</dev/null >{shlex.quote(events)} 2>{shlex.quote(errors)}"
            ),
            env=env,
        )

    @override
    def populate_context_post_run(self, context: AgentContext) -> None:
        path = self.logs_dir / _EVENTS_FILENAME
        if not path.exists():
            return
        usage: dict[str, int] | None = None
        for line in path.read_text(errors="replace").splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if isinstance(event, dict) and event.get("type") == "done" and isinstance(event.get("usage"), dict):
                usage = event["usage"]
        if usage is None:
            return
        cache_read = usage.get("cache_read_input_tokens") or 0
        context.n_cache_tokens = cache_read
        context.n_input_tokens = (usage.get("input_tokens") or 0) + (usage.get("cache_creation_input_tokens") or 0) + cache_read
        context.n_output_tokens = usage.get("output_tokens") or 0
