"""Run the native benchmark and fail when a trial has an execution error."""

import argparse
import asyncio
import os
from pathlib import Path

from harbor.job import Job
from harbor.models.job.config import JobConfig


async def run(config: JobConfig) -> int:
    job = await Job.create(config)
    result = await job.run()
    # Full trial errors can contain sealed task text. Print only aggregate data.
    print(result.stats.model_dump_json(indent=2))
    return 1 if result.stats.n_errored_trials else 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("config", type=Path)
    arguments = parser.parse_args()
    text = arguments.config.read_text()
    # Each job names the upstream key its recorder needs.
    for variable in ("OPENROUTER_API_KEY", "PRIME_API_KEY"):
        if "${" + variable + "}" in text and not os.environ.get(variable):
            parser.error(f"{variable} is required")
    config = JobConfig.model_validate_json(text)
    return asyncio.run(run(config))


if __name__ == "__main__":
    raise SystemExit(main())
