"""Is the provider of the model fast enough for a fair run?

An overloaded provider answers with 429 and with timeouts of five minutes. Each harness
meets those errors in its own way: one retries, another ends the run. A run in those hours
measures the provider and the retry code more than the coding of the harness, and it costs
more, because a trial that waits for timeouts runs long.

Two readings tell the state of the provider.

The throughput that OpenRouter reports for each provider of a model, in tokens per second over
its last 30 minutes. Its 90th percentile is the slow tail: 90% of the requests were at least
that fast. The limits below come from the valid airgapped runs of October 18 and 19, where the
median throughput of a turn of Think was 48 to 53 and its slow tail (the 10th percentile of its
own turns) was 31 to 34. On October 19 at 15:00 UTC the same provider reported 21 and 3, and the
first batch of new tasks ran with 20 and 2, and failed with timeouts and 429s. This reading
catches a bad overload and nothing finer. It averages all traffic over 30 minutes and it lags.
At 17:30 UTC it said 41 and 28, and the gate passed. The next 19 minutes of trials met a
provider that served 83% to 86% of the input tokens from its prompt cache, where every valid job
had 97%, and an agent turn took about three times as long.

The share of input tokens that the provider served from its prompt cache, over the finished
trials of a job. It is the direct reading. A cached token costs 4.5 times less than a new one,
so it moves the dollar cost, and it moves the cost of each harness by a different amount: in
the slow hours Think and the baseline fell from 96% to 78% and 74%, and Claude Code from 98% to
90%. After 15 finished trials, the running ratio of every valid job was at least 94.8%. Both
stopped jobs of October 19 were at 87.8% or less at every point. A job is valid when it ends at
95% or more, and the guard stops a job under 92% once 20 trials have finished.

  python -m evals.provider_health               # the gate: exit 1 when the provider is degraded
  python -m evals.provider_health --trials JOB  # provider errors and cache use of a job; exit 1 when it is not valid
"""

import argparse
from collections.abc import Sequence
from dataclasses import dataclass
from datetime import datetime
import json
import os
from pathlib import Path
import re
import sys
import urllib.error
import urllib.request

MIN_MEDIAN_THROUGHPUT = 40.0
MIN_SLOW_TAIL_THROUGHPUT = 20.0
# The share of input tokens that the provider served from its prompt cache. See the docstring for where
# the limits come from. A job that is running can show its first trials short, with a low share, so its
# limit is lower than the limit of a finished job.
MIN_CACHE_HIT_WHILE_RUNNING = 0.92
MIN_CACHE_HIT_FINISHED = 0.95
MIN_FINISHED_TRIALS = 20
# What an agent writes when the provider fails, in the logs of every harness. The upstream form is what OpenRouter
# relays when the server behind it fails, for example DeepInfra's "finish_reason ... 'abort'". The last line is a
# key or an account that cannot pay or log in: on October 20 the key reached its total limit of $500, and
# 37 trials of one tranche ended on "Key limit exceeded", seven of them in Pi without an exception.
# A rejection that the provider repeats for the same request, such as Codex's "unexpected status 422" after it
# attached an image, is not a failure of the provider and is not here.
PROVIDER_ERROR_PATTERN = re.compile(
    r"overloaded_error|rate_limit_exceeded|temporarily rate-limited|Provider timed out|engine_overloaded"
    r"|Upstream error from|unexpected status 5\d\d|API Error: 5\d\d"
    r"|Key limit exceeded|Insufficient credits|requires more credits|(?:unexpected status|answered|API Error:) 40[123]\b"
)


@dataclass(frozen=True)
class Endpoint:
    """One provider of a model, with the throughput of its last 30 minutes in tokens per second."""

    provider: str
    median_throughput: float
    slow_tail_throughput: float


def judge(endpoint: Endpoint) -> tuple[str, ...]:
    """Why the provider is too slow for a fair run. An empty result means that it is fast enough."""
    reasons: list[str] = []
    if endpoint.median_throughput < MIN_MEDIAN_THROUGHPUT:
        reasons.append(f"the median throughput is {endpoint.median_throughput:.0f} tokens per second; the limit is {MIN_MEDIAN_THROUGHPUT:.0f}")
    if endpoint.slow_tail_throughput < MIN_SLOW_TAIL_THROUGHPUT:
        reasons.append(f"the slow tail is {endpoint.slow_tail_throughput:.0f} tokens per second; the limit is {MIN_SLOW_TAIL_THROUGHPUT:.0f}")
    return tuple(reasons)


def parse_endpoints(payload: dict[str, object]) -> list[Endpoint]:
    """The endpoints of the reply of GET /api/v1/models/{id}/endpoints. An endpoint without throughput figures,
    or a reply of another shape, is an error."""
    try:
        entries = payload["data"]["endpoints"]  # type: ignore[index]
        endpoints = []
        for entry in entries:
            throughput = entry.get("throughput_last_30m")
            if not isinstance(throughput, dict) or "p50" not in throughput or "p90" not in throughput:
                raise ValueError(f"The endpoint {entry.get('provider_name')!r} reports no throughput for the last 30 minutes")
            endpoints.append(Endpoint(str(entry["provider_name"]), float(throughput["p50"]), float(throughput["p90"])))
    except (KeyError, TypeError, AttributeError) as error:
        raise ValueError(f"The reply of the endpoints call has an unexpected shape: {error!r}") from error
    if not endpoints:
        raise ValueError("The model has no endpoint")
    return endpoints


def fetch_endpoints(model: str, key: str) -> list[Endpoint]:
    request = urllib.request.Request(f"https://openrouter.ai/api/v1/models/{model}/endpoints", headers={"Authorization": f"Bearer {key}"})
    with urllib.request.urlopen(request, timeout=30) as response:
        return parse_endpoints(json.load(response))


def check(model: str, key: str) -> str | None:
    """None when every provider of the model is fast enough. Otherwise why not. A status that cannot be
    read is a reason, because a run must not start on a provider of unknown speed."""
    try:
        endpoints = fetch_endpoints(model, key)
    except (OSError, ValueError) as error:
        return f"the status of the provider cannot be read: {error}"
    problems = [f"{endpoint.provider}: {reason}" for endpoint in endpoints for reason in judge(endpoint)]
    return "; ".join(problems) if problems else None


def met_provider_error(trial: Path) -> bool:
    """Whether a trial met a provider error: Harbor ended it with a rate-limit error, or the logs of its agent
    hold the pattern. A harness that retried and went on holds it as well, and its trial ran against the same failing provider."""
    result_path = trial / "result.json"
    exception = (json.loads(result_path.read_text()).get("exception_info") or {}) if result_path.exists() else {}
    if exception.get("exception_type") == "ApiRateLimitError" or PROVIDER_ERROR_PATTERN.search(exception.get("exception_message") or ""):
        return True
    for path in sorted((trial / "agent").glob("*")):
        if path.is_file() and path.suffix in (".txt", ".log", ".jsonl") and PROVIDER_ERROR_PATTERN.search(path.read_text(errors="replace")):
            return True
    return False


def trials_with_provider_errors(job: Path) -> dict[str, list[str]]:
    """The trials of a job that met a provider error, by harness."""
    found: dict[str, list[str]] = {}
    for result_path in sorted(job.glob("*__*/result.json")):
        if met_provider_error(result_path.parent):
            harness = json.loads(result_path.read_text())["agent_info"]["name"]
            found.setdefault(harness, []).append(result_path.parent.name)
    return found


@dataclass(frozen=True)
class CacheUse:
    """The prompt-cache use of the finished trials of some jobs. It counts the trials that reported usage,
    their input tokens (cached tokens included), and their cached tokens."""

    trials: int
    input_tokens: int
    cached_tokens: int

    @property
    def hit_ratio(self) -> float | None:
        """The share of input tokens that came from the cache. None when no input token was counted."""
        return self.cached_tokens / self.input_tokens if self.input_tokens else None


def read_cache_use(jobs: Sequence[Path], since: datetime | None = None) -> CacheUse:
    """The cache use of the finished trials of some jobs. With since, a timezone-aware time, only the trials that
    finished at or after it count, so that a job that was resumed is judged on its new trials and not on the old ones."""
    trials = input_tokens = cached_tokens = 0
    for job in jobs:
        for result_path in sorted(job.glob("*__*/result.json")):
            result = json.loads(result_path.read_text())
            if since is not None and (not result.get("finished_at") or datetime.fromisoformat(result["finished_at"]) < since):
                continue
            agent = result.get("agent_result") or {}
            counted, cached = agent.get("n_input_tokens"), agent.get("n_cache_tokens")
            if not counted or cached is None:
                continue  # A trial that reported no usage counts for nothing, and not as a trial with no cache hits.
            trials += 1
            input_tokens += counted
            cached_tokens += cached
    return CacheUse(trials, input_tokens, cached_tokens)


def cache_problem(use: CacheUse, *, minimum_ratio: float, minimum_trials: int = MIN_FINISHED_TRIALS) -> str | None:
    """None when the cache works, or when too few trials have finished to tell. Otherwise why not."""
    ratio = use.hit_ratio
    if ratio is None or use.trials < minimum_trials:
        return None
    if ratio < minimum_ratio:
        return f"the provider served {100 * ratio:.1f}% of the input tokens of {use.trials} finished trials from its cache; the limit is {100 * minimum_ratio:.0f}%"
    return None


def finished_job_problem(use: CacheUse) -> str | None:
    """Why the cache use of a finished job is not valid. Every trial counts, however few there are, and a job
    with no usage at all is a problem, because its cache use is unknown."""
    if use.hit_ratio is None:
        return "no trial reported usage, so the cache use is unknown"
    return cache_problem(use, minimum_ratio=MIN_CACHE_HIT_FINISHED, minimum_trials=1)


def main() -> int:
    from evals.harbor_wire import MODEL

    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--model", default=MODEL)
    parser.add_argument("--trials", type=Path, action="append", default=[], help="a job folder; report its trials that met a provider error and its cache use, instead of checking the provider")
    arguments = parser.parse_args()
    if arguments.trials:
        invalid = False
        for job in arguments.trials:
            found = trials_with_provider_errors(job)
            total = len(list(job.glob("*__*/result.json")))
            use = read_cache_use([job])
            problem = finished_job_problem(use)
            invalid = invalid or bool(found) or problem is not None
            ratio = "n/a" if use.hit_ratio is None else f"{100 * use.hit_ratio:.1f}%"
            print(f"{job.name}: {sum(len(v) for v in found.values())} of {total} trials met a provider error {({h: len(v) for h, v in sorted(found.items())})}; cache hit {ratio} over {use.trials} trials: " + ("ok" if problem is None else "INVALID, " + problem))
        return 1 if invalid else 0
    key = os.environ.get("OPENROUTER_API_KEY")
    if not key:
        print("provider_health: OPENROUTER_API_KEY is not set", file=sys.stderr)
        return 2
    try:
        endpoints = fetch_endpoints(arguments.model, key)
    except (OSError, ValueError) as error:
        print(f"provider_health FAILED: {error}", file=sys.stderr)
        return 2
    degraded = False
    for endpoint in endpoints:
        reasons = judge(endpoint)
        degraded = degraded or bool(reasons)
        print(f"{endpoint.provider}: median {endpoint.median_throughput:.0f}, slow tail {endpoint.slow_tail_throughput:.0f} tokens per second; " + ("fast enough" if not reasons else "DEGRADED: " + "; ".join(reasons)))
    return 1 if degraded else 0


if __name__ == "__main__":
    raise SystemExit(main())
