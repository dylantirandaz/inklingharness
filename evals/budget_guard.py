"""Stop benchmark runs once their real spend reaches a dollar limit.

Spend is measured, not estimated: OpenRouter key usage (inference) plus the
drop in the Prime wallet balance (sandboxes and images) since the guard
started, plus --already for money spent before it. At the limit the guard
sends SIGTERM to the process group of every PID file it watches.

  python out/terminal-bench/with_key.py evals.budget_guard --limit 400 --already 1.73 out/terminal-bench/tb4-*.pid
"""

import argparse
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import time
import urllib.request


def openrouter_usage() -> float:
    request = urllib.request.Request("https://openrouter.ai/api/v1/key", headers={"Authorization": "Bearer " + os.environ["OPENROUTER_API_KEY"]})
    with urllib.request.urlopen(request, timeout=30) as response:
        return float(json.load(response)["data"]["usage"])


def prime_balance() -> float:
    output = subprocess.run(["prime", "wallet"], capture_output=True, text=True, timeout=60, check=True).stdout
    match = re.search(r"Balance:\s*\$([0-9,.]+)", output)
    if match is None:
        raise ValueError("prime wallet printed no balance")
    return float(match.group(1).replace(",", ""))


def stop(pid_files: list[Path]) -> list[int]:
    stopped = []
    for path in pid_files:
        try:
            pid = int(path.read_text().strip())
            os.killpg(os.getpgid(pid), signal.SIGTERM)
            stopped.append(pid)
        except (OSError, ValueError):
            continue
    return stopped


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--limit", type=float, required=True, help="Total dollars, including --already")
    parser.add_argument("--already", type=float, default=0.0, help="Dollars spent before the guard started")
    parser.add_argument("--interval", type=float, default=120.0)
    parser.add_argument("--once", action="store_true", help="Print the current spend and exit")
    parser.add_argument("pid_files", nargs="*", type=Path)
    arguments = parser.parse_args(argv)
    start_usage, start_balance = openrouter_usage(), prime_balance()
    while True:
        inference = openrouter_usage() - start_usage
        sandboxes = start_balance - prime_balance()
        spent = arguments.already + inference + sandboxes
        print(json.dumps({"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "spent": round(spent, 2), "inference": round(inference, 2), "sandboxes": round(sandboxes, 2), "limit": arguments.limit}), flush=True)
        if arguments.once:
            return 0
        if spent >= arguments.limit:
            print(json.dumps({"stopped": stop(arguments.pid_files)}), flush=True)
            return 1
        if arguments.pid_files and not any(_alive(path) for path in arguments.pid_files):
            return 0
        time.sleep(arguments.interval)


def _alive(path: Path) -> bool:
    try:
        os.kill(int(path.read_text().strip()), 0)
        return True
    except (OSError, ValueError):
        return False


if __name__ == "__main__":
    raise SystemExit(main())
