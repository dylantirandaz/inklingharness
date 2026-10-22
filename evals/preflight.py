"""Preflight for a benchmark run: the API key must be confined to the model under test.

An agent can read the API key from its environment and call any model through
the model API, for example a stronger one that reads an image or solves the
task. A guardrail on the key that allows one model closes that path. OpenRouter
lists, at /api/v1/models/user, only the models that the key may use, so this
check costs nothing: it fails unless that list is exactly the allowed model.

  python out/terminal-bench/with_key.py evals.preflight
"""

from dataclasses import dataclass
import json
import os
import sys
import urllib.request

ALLOWED_MODEL = "thinkingmachines/inkling-small"
MODELS_URL = "https://openrouter.ai/api/v1/models/user"

SETUP = """Create a guardrail in the OpenRouter workspace (Workspaces > Default > Guardrails):
  - Model allowlist: {model}
  - Budget limit: a daily cap above the cost of the run
Create a new API key for the benchmark, assign the guardrail under the API Keys section of the
guardrail, and store that key (think login), so that the run uses it."""


@dataclass(frozen=True)
class Confined:
    model: str


@dataclass(frozen=True)
class NotConfined:
    extra_models: int
    examples: tuple[str, ...]


@dataclass(frozen=True)
class AllowedModelMissing:
    listed: int


Verdict = Confined | NotConfined | AllowedModelMissing


def judge(listed: list[str], allowed: str = ALLOWED_MODEL) -> Verdict:
    """Decide from the models that the key may use."""
    if allowed not in listed:
        return AllowedModelMissing(listed=len(listed))
    extra = sorted(model for model in listed if model != allowed)
    if extra:
        return NotConfined(extra_models=len(extra), examples=tuple(extra[:3]))
    return Confined(model=allowed)


def fetch_listed_models(key: str) -> list[str]:
    request = urllib.request.Request(MODELS_URL, headers={"Authorization": f"Bearer {key}"})
    with urllib.request.urlopen(request, timeout=30) as response:
        return [entry["id"] for entry in json.load(response)["data"]]


def failure_message(verdict: Verdict) -> str | None:
    """None when the key is confined. Otherwise why it is not, and how to confine it."""
    match verdict:
        case Confined():
            return None
        case NotConfined(extra, examples):
            return f"the key can use {extra} other models, for example {', '.join(examples)}.\n" + SETUP.format(model=ALLOWED_MODEL)
        case AllowedModelMissing(listed):
            return f"the key lists {listed} models and {ALLOWED_MODEL} is not one of them.\n" + SETUP.format(model=ALLOWED_MODEL)


def check_environment_key() -> str | None:
    """Check the key in OPENROUTER_API_KEY. None means it is confined; otherwise the reason."""
    key = os.environ.get("OPENROUTER_API_KEY")
    if not key:
        return "OPENROUTER_API_KEY is not set"
    return failure_message(judge(fetch_listed_models(key)))


def main() -> int:
    problem = check_environment_key()
    if problem is None:
        print(f"preflight: the key can use only {ALLOWED_MODEL}")
        return 0
    print(f"preflight FAILED: {problem}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
