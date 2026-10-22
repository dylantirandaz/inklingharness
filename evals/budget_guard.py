"""Stop benchmark runs once their real spend reaches a dollar limit.

Spend uses OpenRouter key usage and decreases in the Prime wallet balance.
The --already amount carries earlier spend forward. Wallet top-ups do not
reduce recorded spend. The guard stops the watched process groups at the
limit, on a balance read failure, when too many owned sandboxes remain, and
when the account credits or the key's own limit fall below --min-credits.
The key limit matters as much as the credits: on October 20 a key with a
total limit of $500 reached it in the middle of a job, and OpenRouter then
answered 403 to every request while the account still held credits.

With --backend docker the trials run in local Docker: there is no wallet to
read, and the guard instead removes unused Terminal-Bench images when free
disk falls below --min-disk-gb, and removes trial containers when it stops.

With --backend modal the trials run in Modal VMs: there is no wallet to read.
Stopping a job leaves its sandboxes running, and their agents keep spending
credit, so the guard terminates the sandboxes of the watched jobs' trials.

  python out/terminal-bench/with_key.py evals.budget_guard --limit 395 --already 107.23 --max-sandboxes 10 --job-dir JOB_DIR PID_FILE
"""

import argparse
import asyncio
from dataclasses import dataclass
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import time
import urllib.request

from evals.harbor_wire import MODEL
from evals.provider_health import MIN_CACHE_HIT_WHILE_RUNNING, cache_problem, read_cache_use
from evals.provider_health import check as check_provider

MODAL_APP = "inkling-terminal-bench"
# A read inside the loop is tried this many times, this many seconds apart, before the guard takes a
# failure for an outage and stops the jobs. One connection reset on October 20 ended a healthy run of 200 trials.
LOOP_READ_ATTEMPTS = 5
LOOP_READ_PAUSE = 10.0


@dataclass(frozen=True)
class KeyStatus:
    usage: float
    # None when the key has no limit of its own.
    limit_remaining: float | None


def openrouter_key() -> KeyStatus:
    request = urllib.request.Request("https://openrouter.ai/api/v1/key", headers={"Authorization": "Bearer " + os.environ["OPENROUTER_API_KEY"]})
    with urllib.request.urlopen(request, timeout=30) as response:
        data = json.load(response)["data"]
    remaining = data["limit_remaining"]
    return KeyStatus(float(data["usage"]), None if remaining is None else float(remaining))


def funds_problem(credits_left: float, key_limit_left: float | None, minimum: float) -> str | None:
    """None when both the account credits and the key's own limit hold at least minimum dollars. Otherwise why not."""
    if credits_left < minimum:
        return "OpenRouter credits exhausted"
    if key_limit_left is not None and key_limit_left < minimum:
        return "the limit of the OpenRouter key is exhausted"
    return None


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


TB_IMAGE = "harborframework/terminal-bench"


def free_disk_gb() -> float:
    stats = os.statvfs(os.path.expanduser("~"))
    return stats.f_bavail * stats.f_frsize / 1e9


def _docker(*args: str, timeout: int = 300) -> str:
    return subprocess.run(["docker", *args], capture_output=True, text=True, timeout=timeout).stdout


def prune_tb_images() -> list[str]:
    """Remove Terminal-Bench images no container uses; Docker refuses to remove one in use."""
    used = set(_docker("ps", "-a", "--format", "{{.Image}}").split())
    removed = []
    for image in _docker("image", "ls", TB_IMAGE, "--format", "{{.Repository}}:{{.Tag}}").split():
        if image not in used and _docker("rmi", image).strip():
            removed.append(image)
    return removed


def remove_tb_containers() -> list[str]:
    ids = [line.split()[0] for line in _docker("ps", "-a", "--format", "{{.ID}} {{.Image}}").splitlines() if TB_IMAGE in line]
    if ids:
        _docker("rm", "-f", *ids)
    return ids


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


def next_degraded_count(problem: str | None, count: int) -> int:
    """The number of degraded provider checks in a row. A healthy check, where problem is None, ends the row."""
    return 0 if problem is None else count + 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--limit", type=float, required=True, help="Total dollars, including --already")
    parser.add_argument("--already", type=float, default=0.0, help="Dollars spent before the guard started")
    parser.add_argument("--min-credits", type=float, default=2.0, help="Stop when the OpenRouter credits, the OpenRouter key's own limit, or the Prime wallet fall below this")
    parser.add_argument("--interval", type=float, default=120.0)
    parser.add_argument("--backend", choices=["prime", "docker", "modal"], default="prime")
    parser.add_argument("--min-disk-gb", type=float, default=15.0, help="Docker backend: prune unused task images below this much free disk")
    parser.add_argument("--once", action="store_true", help="Print the current spend and exit")
    parser.add_argument("--job-dir", action="append", type=Path, default=[], help="Prime or Modal job directory; repeat for each watched job")
    parser.add_argument("--max-sandboxes", type=int, help="Stop if owned Prime sandboxes exceed this count")
    parser.add_argument("--modal-app", default=MODAL_APP, help="Modal backend: the Modal app that holds the trial sandboxes")
    parser.add_argument("--stop-on-degraded-provider", type=int, default=0, metavar="CHECKS", help="Stop when evals.provider_health finds the provider degraded or unreadable in this many checks in a row; 0 turns the check off")
    parser.add_argument("--min-cache-hit", type=float, default=0.0, metavar="RATIO", help=f"Stop when the finished trials of the --job-dir jobs, 20 or more, got less than this share of their input tokens from the prompt cache; 0 turns the check off. {MIN_CACHE_HIT_WHILE_RUNNING} is the limit that evals.provider_health explains. It was set on trials of 30 to 40 turns. Short trials read less cached context, so on a healthy provider Think shows about 89% at low effort (7 turns) and 91% at medium effort (12 turns), and the check must be off for them")
    parser.add_argument("pid_files", nargs="*", type=Path)
    arguments = parser.parse_args(argv)
    prime, docker, modal = (arguments.backend == name for name in ("prime", "docker", "modal"))
    read_balance = prime_balance if prime else (lambda: 0.0)
    if prime and not arguments.once:
        if not arguments.job_dir or not arguments.pid_files or arguments.max_sandboxes is None or arguments.max_sandboxes < 1:
            parser.error("Prime requires --job-dir, --max-sandboxes, and PID files")
    if modal and not arguments.once and (not arguments.job_dir or not arguments.pid_files):
        parser.error("Modal requires --job-dir and PID files")
    try:
        start_usage, start_balance = _retry(openrouter_key).usage, _retry(read_balance)
    except (OSError, ValueError, subprocess.SubprocessError):
        stop(arguments.pid_files)
        if prime and arguments.job_dir:
            time.sleep(60)
            delete_sandboxes(arguments.job_dir)
        elif modal and arguments.job_dir:
            time.sleep(10)
            terminate_modal_sandboxes(arguments.job_dir, arguments.modal_app)
        elif docker:
            remove_tb_containers()
        raise
    previous_balance = start_balance
    started = datetime.now(timezone.utc)
    sandboxes = 0.0
    degraded_checks = 0
    while True:
        try:
            key = _retry(openrouter_key, attempts=LOOP_READ_ATTEMPTS, pause=LOOP_READ_PAUSE)
            inference = key.usage - start_usage
            balance = _retry(read_balance, attempts=LOOP_READ_ATTEMPTS, pause=LOOP_READ_PAUSE)
            sandboxes += max(0.0, previous_balance - balance)
            previous_balance = balance
            credits = _retry(openrouter_credits_left, attempts=LOOP_READ_ATTEMPTS, pause=LOOP_READ_PAUSE)
            if arguments.once:
                owned = []
            elif prime:
                owned = sandbox_ids(arguments.job_dir)
            elif modal:
                owned = _retry(lambda: modal_sandboxes(arguments.job_dir, arguments.modal_app), attempts=LOOP_READ_ATTEMPTS, pause=LOOP_READ_PAUSE)
            else:
                owned = []
            jobs_finished = bool(arguments.pid_files) and not any(_alive(path) for path in arguments.pid_files)
        except (OSError, ValueError, subprocess.SubprocessError) as error:
            print(json.dumps({"read_error": str(error), "stopped": stop(arguments.pid_files)}), flush=True)
            if prime:
                time.sleep(60)
                delete_sandboxes(arguments.job_dir)
            elif modal:
                time.sleep(10)
                terminate_modal_sandboxes(arguments.job_dir, arguments.modal_app)
            elif docker:
                remove_tb_containers()
            raise
        problem = check_provider(MODEL, os.environ["OPENROUTER_API_KEY"]) if arguments.stop_on_degraded_provider and not arguments.once else None
        degraded_checks = next_degraded_count(problem, degraded_checks)
        cache_use = read_cache_use(arguments.job_dir, since=started) if arguments.min_cache_hit and not arguments.once else None
        cache_issue = None if cache_use is None else cache_problem(cache_use, minimum_ratio=arguments.min_cache_hit)
        spent = arguments.already + inference + sandboxes
        record = {"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "spent": round(spent, 2), "inference": round(inference, 2), "limit": arguments.limit, "openrouter_credits_left": round(credits, 2)}
        if key.limit_remaining is not None:
            record["openrouter_key_limit_left"] = round(key.limit_remaining, 2)
        if docker:
            record["free_disk_gb"] = round(free_disk_gb(), 1)
            if record["free_disk_gb"] < arguments.min_disk_gb and not arguments.once:
                try:
                    record["pruned_images"] = prune_tb_images()
                except (OSError, subprocess.SubprocessError) as error:
                    record["prune_error"] = str(error)
        elif prime:
            record.update(sandboxes=round(sandboxes, 2), prime_balance=round(balance, 2), owned_sandboxes=len(owned))
        elif modal:
            record.update(owned_sandboxes=len(owned))
        if arguments.stop_on_degraded_provider:
            record["provider_degraded_checks"] = degraded_checks
        if cache_use is not None and cache_use.hit_ratio is not None:
            record.update(cache_hit_ratio=round(cache_use.hit_ratio, 4), cache_trials=cache_use.trials)
        print(json.dumps(record), flush=True)
        if arguments.once:
            return 0
        reason = None
        if spent >= arguments.limit:
            reason = "budget limit reached"
        elif (funds := funds_problem(credits, key.limit_remaining, arguments.min_credits)) is not None:
            reason = funds
        elif prime and balance < arguments.min_credits:
            reason = "Prime wallet exhausted"
        elif prime and len(owned) > arguments.max_sandboxes:
            reason = "owned sandbox count exceeds the trial limit"
        elif arguments.stop_on_degraded_provider and degraded_checks >= arguments.stop_on_degraded_provider:
            reason = f"the provider is degraded: {problem}"
        elif cache_issue is not None:
            reason = f"the prompt cache is not working: {cache_issue}"
        if reason is not None or jobs_finished:
            print(json.dumps({"reason": reason or "all jobs stopped", "stopped": stop(arguments.pid_files)}), flush=True)
            # Each minute that the sandboxes live costs credit. Prime needs a pause before its deletion call.
            time.sleep(5 if modal else 60)
            if docker:
                print(json.dumps({"removed_containers": remove_tb_containers()}), flush=True)
            elif prime:
                print(json.dumps({"deleted_sandboxes": delete_sandboxes(arguments.job_dir)}), flush=True)
            elif modal:
                print(json.dumps({"terminated_sandboxes": terminate_modal_sandboxes(arguments.job_dir, arguments.modal_app)}), flush=True)
            return 1 if reason is not None else 0
        time.sleep(arguments.interval)


def sandbox_ids(job_dirs: list[Path]) -> list[str]:
    """List only sandboxes whose names belong to these jobs' saved trials."""
    prefixes = tuple(
        f"harbor-{trial.name}__"
        for job_dir in job_dirs if job_dir.exists()
        for trial in job_dir.iterdir()
        if trial.is_dir() and (trial / "config.json").is_file()
    )
    if not prefixes:
        return []
    from prime_sandboxes import AsyncSandboxClient
    from prime_sandboxes.core.client import APIError

    async def read_owned_ids() -> list[str]:
        async with AsyncSandboxClient() as client:
            inventory = await asyncio.wait_for(
                client.list(exclude_terminated=True, per_page=500), timeout=30,
            )
        if inventory.has_next:
            raise ValueError("Prime inventory exceeds 500 entries; cannot verify job ownership")
        return [
            box.id for box in inventory.sandboxes
            if box.name.startswith(prefixes) and box.status != "TERMINATED"
        ]

    try:
        return asyncio.run(read_owned_ids())
    except APIError as error:
        raise OSError(f"Prime inventory read failed: {error}") from error
    except TimeoutError as error:
        raise OSError("Prime inventory did not respond within 30 seconds.") from error


def delete_sandboxes(job_dirs: list[Path]) -> list[str]:
    """Delete owned sandboxes and fail if deletion does not complete."""
    ids = sandbox_ids(job_dirs)
    if not ids:
        return []
    subprocess.run(["prime", "sandbox", "delete", "-y", "--plain", *ids], capture_output=True, text=True, timeout=600, check=True)
    deadline = time.monotonic() + 120
    while remaining := sandbox_ids(job_dirs):
        if time.monotonic() >= deadline:
            raise RuntimeError(f"Prime sandboxes remain after deletion: {remaining}")
        time.sleep(2)
    return ids


def modal_session_prefixes(job_dirs: list[Path]) -> tuple[str, ...]:
    """Harbor names the sandboxes of a trial "<trial>__env" and
    "<trial>__verifier__...". The closing "__" keeps a trial from matching
    another trial whose name starts the same way."""
    return tuple(
        f"{trial.name}__"
        for job_dir in job_dirs if job_dir.exists()
        for trial in job_dir.iterdir()
        if trial.is_dir() and (trial / "config.json").is_file()
    )


def modal_sandboxes(job_dirs: list[Path], app_name: str) -> list:
    """List the running Modal sandboxes that belong to these jobs' saved trials.
    A lost or refused connection to Modal is an OSError, like the failed reads of OpenRouter: grpclib and the
    Modal client raise their own classes, and on October 20 an unhandled StreamTerminatedError ended the guard
    while three jobs went on for an hour with no limit on their spend."""
    prefixes = modal_session_prefixes(job_dirs)
    if not prefixes:
        return []
    import grpclib.exceptions
    import modal

    try:
        app = modal.App.lookup(app_name, create_if_missing=False)
        return [
            sandbox
            for sandbox in modal.Sandbox.list(app_id=app.app_id, tags={"harbor.managed": "true"})
            if sandbox.get_tags().get("harbor.session_id", "").startswith(prefixes)
        ]
    except modal.exception.NotFoundError:
        # The app exists once the first trial has started a sandbox.
        return []
    except (grpclib.exceptions.GRPCError, grpclib.exceptions.StreamTerminatedError, modal.exception.Error) as error:
        raise OSError(f"cannot list the Modal sandboxes: {error!r}") from error


def terminate_modal_sandboxes(job_dirs: list[Path], app_name: str) -> list[str]:
    """Terminate owned sandboxes and fail if termination does not complete."""
    owned = modal_sandboxes(job_dirs, app_name)
    if not owned:
        return []
    ids = [sandbox.object_id for sandbox in owned]
    for sandbox in owned:
        sandbox.terminate()
    deadline = time.monotonic() + 120
    while remaining := modal_sandboxes(job_dirs, app_name):
        if time.monotonic() >= deadline:
            raise RuntimeError(f"Modal sandboxes remain after termination: {[sandbox.object_id for sandbox in remaining]}")
        time.sleep(2)
    return ids



def _retry(read, attempts: int = 10, pause: float = 15.0):
    """Call read until it succeeds. A failure on the last attempt is raised."""
    for attempt in range(attempts):
        try:
            return read()
        except (OSError, ValueError, subprocess.SubprocessError):
            if attempt == attempts - 1:
                raise
            time.sleep(pause)


def _alive(path: Path) -> bool:
    """Return false for missing or exited processes; fail if ps cannot read state."""
    try:
        pid = int(path.read_text().strip())
    except (OSError, ValueError):
        return False
    result = subprocess.run(
        ["ps", "-p", str(pid), "-o", "stat="],
        capture_output=True, text=True, timeout=5,
    )
    if result.returncode == 1:
        return False
    result.check_returncode()
    state = result.stdout.strip()
    if not state:
        raise ValueError(f"ps returned no state for process {pid}")
    return state[0] not in ("Z", "X")


if __name__ == "__main__":
    raise SystemExit(main())
