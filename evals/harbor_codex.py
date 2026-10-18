"""Harbor's Codex agent, pointed at an OpenRouter model slug.

The built-in agent sends only the text after the last slash as the model name:
`inkling-small` for `thinkingmachines/inkling-small`. OpenRouter needs the whole
slug. This class changes that and nothing else. Prompts, tools, config, and the
command line stay those of the built-in agent.
"""

from harbor.agents.installed.codex import Codex


class _WholeModelName(str):
    """A model name that the built-in agent cannot cut at a slash."""

    def split(self, separator=None, maxsplit=-1):
        return [str(self)]


class CodexOpenRouter(Codex):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        # The base class has read the provider and model from the name by now.
        if self.model_name:
            self.model_name = _WholeModelName(self.model_name)
