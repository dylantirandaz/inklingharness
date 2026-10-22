"""Run the native benchmark and fail when a trial has an execution error."""

import argparse
import asyncio
import os
from pathlib import Path

from harbor.job import Job
from harbor.models.job.config import JobConfig

from evals.harbor_wire import MODEL
from evals.preflight import check_environment_key
from evals.provider_health import check as check_provider


async def run(config: JobConfig) -> int:
    job = await Job.create(config)
    result = await job.run()
    # Full trial errors can contain sealed task text. Print only aggregate data.
    print(result.stats.model_dump_json(indent=2))
    return 1 if result.stats.n_errored_trials else 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("config", type=Path)
    parser.add_argument("--require-confined-key", action="store_true", help="refuse to start unless the API key can use only the model under test; see evals.preflight")
    parser.add_argument("--require-healthy-provider", action="store_true", help="refuse to start while the provider of the model is slow or overloaded; see evals.provider_health")
    arguments = parser.parse_args()
    if arguments.require_confined_key and (problem := check_environment_key()) is not None:
        parser.error(problem)
    if arguments.require_healthy_provider and (problem := check_provider(MODEL, os.environ.get("OPENROUTER_API_KEY", ""))) is not None:
        parser.error(problem)
    text = arguments.config.read_text()
    # Each job names the upstream key its recorder needs.
    for variable in ("OPENROUTER_API_KEY", "PRIME_API_KEY"):
        if "${" + variable + "}" in text and not os.environ.get(variable):
            parser.error(f"{variable} is required")
    config = JobConfig.model_validate_json(text)
    # Docker Compose resolves relative host paths against the task's folder,
    # so artifact copies to a relative jobs_dir land nowhere.
    config.jobs_dir = Path(config.jobs_dir).resolve()
    return asyncio.run(run(config))


if __name__ == "__main__":
    raise SystemExit(main())
