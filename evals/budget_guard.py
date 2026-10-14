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


def openrouter_credits_left() -> float:
    # Account credits, shared by every key: a key under its own limit still
    # gets HTTP 402 once these run out, and then every trial fails at once.
    request = urllib.request.Request("https://openrouter.ai/api/v1/credits", headers={"Authorization": "Bearer " + os.environ["OPENROUTER_API_KEY"]})
    with urllib.request.urlopen(request, timeout=30) as response:
        data = json.load(response)["data"]
    return float(data["total_credits"]) - float(data["total_usage"])


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
    parser.add_argument("--min-credits", type=float, default=2.0, help="Stop when OpenRouter credits or the Prime wallet fall below this")
    parser.add_argument("--interval", type=float, default=120.0)
    parser.add_argument("--once", action="store_true", help="Print the current spend and exit")
    parser.add_argument("pid_files", nargs="*", type=Path)
    arguments = parser.parse_args(argv)
    started = time.strftime("%Y-%m-%d %H:%M:%S", time.gmtime())
    start_usage, start_balance = _retry(openrouter_usage), _retry(prime_balance)
    while True:
        try:
            inference = openrouter_usage() - start_usage
            balance = prime_balance()
            sandboxes = start_balance - balance
            credits = openrouter_credits_left()
        except (OSError, ValueError, subprocess.SubprocessError) as error:
            # A network blip must not end the guard while jobs keep spending.
            print(json.dumps({"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "read_error": str(error)}), flush=True)
            time.sleep(min(arguments.interval, 30.0))
            continue
        spent = arguments.already + inference + sandboxes
        print(json.dumps({"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "spent": round(spent, 2), "inference": round(inference, 2), "sandboxes": round(sandboxes, 2), "limit": arguments.limit, "openrouter_credits_left": round(credits, 2), "prime_balance": round(balance, 2)}), flush=True)
        if arguments.once:
            return 0
        reason = None
        if spent >= arguments.limit:
            reason = "budget limit reached"
        elif credits < arguments.min_credits:
            reason = "OpenRouter credits exhausted"
        elif balance < arguments.min_credits:
            reason = "Prime wallet exhausted"
        if reason is not None:
            print(json.dumps({"reason": reason, "stopped": stop(arguments.pid_files)}), flush=True)
            time.sleep(60)
            print(json.dumps({"deleted_sandboxes": delete_sandboxes(started)}), flush=True)
            return 1
        if arguments.pid_files and not any(_alive(path) for path in arguments.pid_files):
            return 0
        time.sleep(arguments.interval)


def delete_sandboxes(since: str) -> list[str]:
    """Delete Harbor sandboxes created since the guard started; killed jobs leave them running and billing."""
    try:
        listed = subprocess.run(["prime", "sandbox", "list", "-o", "json", "-n", "500"], capture_output=True, text=True, timeout=120, check=True).stdout
        ids = [box["id"] for box in json.loads(listed)["sandboxes"] if box.get("name", "").startswith("harbor-") and box.get("created_at", "") >= since and box.get("status") != "TERMINATED"]
        if ids:
            subprocess.run(["prime", "sandbox", "delete", "-y", "--plain", *ids], capture_output=True, text=True, timeout=600, check=True)
        return ids
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        print(json.dumps({"sandbox_cleanup_error": str(error)}), flush=True)
        return []


def _retry(read):
    for attempt in range(10):
        try:
            return read()
        except (OSError, ValueError, subprocess.SubprocessError):
            if attempt == 9:
                raise
            time.sleep(15)


def _alive(path: Path) -> bool:
    try:
        os.kill(int(path.read_text().strip()), 0)
        return True
    except (OSError, ValueError):
        return False


if __name__ == "__main__":
    raise SystemExit(main())
