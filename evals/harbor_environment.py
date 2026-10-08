"""Keep native Terminal-Bench runs on the task's declared CPU and memory limits."""

import importlib.metadata
import json
from pathlib import Path
import shlex

from harbor.environments.modal import ModalEnvironment
from harbor.models.trial.config import ResourceMode


class NativeModalEnvironment(ModalEnvironment):
    async def start(self, force_build: bool) -> None:
        if not self._vm_runtime_enabled:
            raise ValueError("Native Terminal-Bench runs require modal_vm_runtime=true")
        if self._cpu_resource_mode != ResourceMode.GUARANTEE or self._memory_resource_mode != ResourceMode.GUARANTEE:
            raise ValueError("Matched native runs require CPU and memory policy 'guarantee'")
        if self._override_cpus is not None or self._override_memory_mb is not None or self._override_storage_mb is not None or self._override_gpus is not None or self._override_tpu is not None:
            raise ValueError("Matched native runs must keep the task's declared resources")
        await super().start(force_build)
        script = Path(__file__).with_name("harbor_preflight.sh").read_text()
        result = await self.exec(
            shlex.join(["bash", "-c", script, "native-preflight", str(self.task_env_config.cpus), str(self.task_env_config.memory_mb)]),
            timeout_sec=30,
        )
        record = {
            "backend": "Modal",
            "runtime": "vm",
            "harbor_version": importlib.metadata.version("harbor"),
            "modal_version": importlib.metadata.version("modal"),
            "image": self.task_env_config.docker_image,
            "cpu_cores": self.task_env_config.cpus,
            "memory_mib": self.task_env_config.memory_mb,
            "cpu_policy": "guarantee",
            "memory_policy": "guarantee",
            "stdout": result.stdout,
            "stderr": result.stderr,
            "exit_code": result.return_code,
        }
        (self.trial_paths.trial_dir / "hardware.json").write_text(json.dumps(record, indent=2) + "\n")
        self.logger.info("Native task resource check: %s", result.stdout)
        if result.return_code != 0:
            raise RuntimeError(f"Native task resource check failed ({result.return_code}): {result.stderr}")
