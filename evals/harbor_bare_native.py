"""The no-harness baseline as a Harbor installed agent, set up like the built-ins.

It mirrors evals/harbor_think.py: the provider key comes from the model
connection, the settings come from agent options, and one CLI command runs for
each task. The binary is built from evals/bare, which sends no system prompt
and keeps no planning, context, or recovery policy. No recorder sits between
the binary and the provider, so the effort and the output limit are set by the
flags below and by nothing else.

The binary comes from a local Linux build, so a run names the exact build by
its SHA-256 digest.

Some task images have no usable CA bundle, and a static Go binary reads only
the system bundle, so it could not verify the provider there. The adapter
therefore uploads the Mozilla bundle that certifi ships, which Harbor already
depends on through httpx, and names it in SSL_CERT_FILE.
"""

import json
import shlex
from typing import Annotated, Literal, override

import certifi
from pydantic import Field

from harbor.agents.capabilities import AgentCapabilities
from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.agents.model_connection import ModelConnectionSpec
from harbor.agents.options import Cli, InstalledAgentOptions
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

from evals.harbor_binary import LocalFile, put_file

_REMOTE_BINARY = "/installed-agent/bare"
_REMOTE_CA_BUNDLE = "/installed-agent/ca-bundle.pem"
_EVENTS_FILENAME = "bare-events.jsonl"
_STDERR_FILENAME = "bare-stderr.log"
_PROVIDER = "openrouter"
_DEFAULT_BASE_URL = "https://openrouter.ai/api/v1"


class BareOptions(InstalledAgentOptions):
    binary_path: str = Field(description="Path of a linux/amd64 build of bare on this computer.")
    staged_dir: str | None = Field(
        default=None,
        description="Folder in the sandbox, on a read-only volume, that holds this build and the CA bundle under their staged names; see evals.harbor_binary. Without it they are uploaded.",
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


class NativeBare(BaseInstalledAgent):
    capabilities = AgentCapabilities()
    MODEL_CONNECTION = ModelConnectionSpec(default_provider=_PROVIDER)
    options_model = BareOptions
    options: BareOptions

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._local = LocalFile.read(self.options.binary_path)
        self._ca_bundle = LocalFile.read(certifi.where())
        with self._local.path.open("rb") as binary:
            header = binary.read(20)
        if header[:6] != b"\x7fELF\x02\x01" or header[18:20] != b"\x3e\x00":
            raise ValueError(f"Expected a Linux amd64 ELF binary: {self._local.path}")
        if self._version is None:
            self._version = "sha256:" + self._local.sha256

    @staticmethod
    @override
    def name() -> str:
        return "bare"

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        await put_file(environment, self._local, _REMOTE_BINARY, mode="755", staged_dir=self.options.staged_dir)
        await put_file(environment, self._ca_bundle, _REMOTE_CA_BUNDLE, mode="644", staged_dir=self.options.staged_dir)

    def _model_id(self) -> str:
        if not self.model_name:
            raise ValueError("The bare agent needs a model name")
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
            raise ValueError("The bare agent needs an OpenRouter model and OPENROUTER_API_KEY")
        # The binary appends /v1/messages to the API root, so it wants the root without /v1.
        base_url = (access.base_url or _DEFAULT_BASE_URL).removesuffix("/v1")
        command = [_REMOTE_BINARY, "-model", self._model_id(), "-base-url", base_url]
        if flags := self.build_cli_flags():
            command.append(flags)
        events = (self.environment_logs_dir / _EVENTS_FILENAME).as_posix()
        errors = (self.environment_logs_dir / _STDERR_FILENAME).as_posix()
        await self.exec_as_agent(
            environment,
            command=(
                f"{' '.join(command)} -- {shlex.quote(instruction)} "
                f"</dev/null >{shlex.quote(events)} 2>{shlex.quote(errors)}"
            ),
            env={"OPENROUTER_API_KEY": access.api_key, "SSL_CERT_FILE": _REMOTE_CA_BUNDLE},
        )

    @override
    def populate_context_post_run(self, context: AgentContext) -> None:
        path = self.logs_dir / _EVENTS_FILENAME
        if not path.exists():
            return
        totals = {"input": 0, "cache": 0, "output": 0}
        counted = False
        for line in path.read_text(errors="replace").splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if not isinstance(event, dict) or event.get("event") != "turn" or not isinstance(event.get("usage"), dict):
                continue
            usage = event["usage"]
            cache_read = usage.get("cache_read_input_tokens") or 0
            totals["input"] += (usage.get("input_tokens") or 0) + (usage.get("cache_creation_input_tokens") or 0) + cache_read
            totals["cache"] += cache_read
            totals["output"] += usage.get("output_tokens") or 0
            counted = True
        if counted:
            context.n_input_tokens = totals["input"]
            context.n_cache_tokens = totals["cache"]
            context.n_output_tokens = totals["output"]
