"""Run native Prime commands and check each task's CPU and memory resources."""

import importlib.metadata
import json
from pathlib import Path
import shlex

from harbor.environments.base import ExecResult
from harbor.environments.prime import PrimeEnvironment


class NativePrimeEnvironment(PrimeEnvironment):
    async def _host_exec(
        self,
        command: str,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
        timeout_sec: int | None = None,
        user: str | int | None = None,
    ) -> ExecResult:
        client, sandbox_id = self._require_sandbox()
        user = user if self._compose else self._resolve_user(user)
        command_env = (env or {}) if self._compose else (self._merge_env(env) or {})
        if user is not None and str(user) not in {"root", "0"}:
            if "HOME" in command_env:
                command = f"export HOME={shlex.quote(command_env['HOME'])}; {command}"
            if isinstance(user, int) or str(user).isdigit():
                command = (
                    f"account=$(id -un {user} 2>/dev/null); "
                    f'if [ -n "$account" ]; then '
                    f'su -s /bin/bash "$account" -c {shlex.quote(command)}; '
                    f"else setpriv --reuid={user} --regid={user} --clear-groups "
                    f"bash -c {shlex.quote(command)}; fi"
                )
            else:
                command = f"su -s /bin/bash {shlex.quote(user)} -c {shlex.quote(command)}"
        command_env = {
            key: value
            for key, value in command_env.items()
            if self._compose or self._persistent_env.get(key) != value
        }
        # Keep output in the VM while short status requests replace the long RPC stream.
        result = await client.run_background_job(
            sandbox_id,
            command,
            working_dir=cwd or ("/" if self._compose else self.task_env_config.workdir),
            env=command_env or None,
            timeout=timeout_sec if timeout_sec is not None else 24 * 60 * 60,
        )
        if result.exit_code is None:
            raise RuntimeError("Prime background job returned no exit code.")
        return ExecResult(
            stdout=result.stdout,
            stderr=result.stderr,
            return_code=result.exit_code,
        )

    async def start(self, force_build: bool) -> None:
        await super().start(force_build)
        try:
            script = Path(__file__).with_name("harbor_preflight.sh").read_text()
            result = await self._host_exec(
                shlex.join(["bash", "-c", script, "native-preflight", "prime", str(self.task_env_config.cpus), str(self.task_env_config.memory_mb)]),
                timeout_sec=30,
                user="root",
            )
            record = {
                "backend": "Prime",
                "runtime": "vm",
                "session_id": self.session_id,
                "sandbox_id": self._sandbox_id,
                "harbor_version": importlib.metadata.version("harbor"),
                "prime_sandboxes_version": importlib.metadata.version("prime-sandboxes"),
                "image": self.task_env_config.docker_image,
                "cpu_cores": self.task_env_config.cpus,
                "memory_mib": self.task_env_config.memory_mb,
                "stdout": result.stdout,
                "stderr": result.stderr,
                "exit_code": result.return_code,
            }
            path = self.trial_paths.trial_dir / f"hardware-{self.session_id}.json"
            path.write_text(json.dumps(record, indent=2) + "\n")
            self.logger.info("Native task resource check: %s", result.stdout)
            if result.return_code != 0:
                raise RuntimeError(f"Prime task resource check failed ({result.return_code}): {result.stderr}")
        except BaseException:
            await self.stop(delete=True)
            raise
