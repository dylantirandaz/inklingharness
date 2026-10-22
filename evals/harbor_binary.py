"""Put a local file into a Harbor sandbox and prove that it arrived whole.

Think and the baseline are Go binaries that Harbor uploads into every sandbox. On October 20, with an
uplink of about 100 KB/s, uploads took 150 to 360 seconds, some timed out, and three binaries crashed
with a segmentation fault seconds after they started. One of them printed bytes of its own memory to
its error log. In the valid runs, with a fast uplink, the upload took about 2 seconds and no binary
crashed. An upload that is not checked can arrive damaged and make an agent fail for a reason that is not
the harness, so every install here compares the SHA-256 of the file in the sandbox with the local one.

There are two ways to get the file in.

  upload   Harbor uploads it from this computer. A file that arrives damaged is uploaded again, and the
           third damaged arrival is an error.
  staged   The file is on a read-only Modal volume, named <file name>-<first 12 digits of its SHA-256>, and
           the sandbox copies it from the mount. Nothing goes through this computer's link, and the name
           says what the file is. A file that is missing, or that does not match, is an error. It is never
           replaced by an upload, because that would hide the fault that staging exists to avoid.

evals.stage_agent_files puts files on the volume.
"""

from dataclasses import dataclass
import hashlib
from pathlib import Path
import shlex
from typing import Protocol

UPLOAD_ATTEMPTS = 3
# Seconds that a sandbox may take to hash or copy one of these files. They are under 10 MB.
COMMAND_TIMEOUT_SECONDS = 120


class SandboxLike(Protocol):
    """The part of a Harbor environment that this module uses."""

    async def upload_file(self, source_path: Path | str, target_path: str) -> None: ...

    async def exec(self, command: str, cwd: str | None = None, env: dict[str, str] | None = None, timeout_sec: int | None = None, user: str | int | None = None): ...


@dataclass(frozen=True)
class LocalFile:
    path: Path
    sha256: str

    @classmethod
    def read(cls, path: Path | str) -> "LocalFile":
        resolved = Path(path).expanduser().resolve(strict=True)
        with resolved.open("rb") as handle:
            return cls(resolved, hashlib.file_digest(handle, "sha256").hexdigest())

    @property
    def staged_name(self) -> str:
        """The name of the file on the staged volume."""
        return f"{self.path.name}-{self.sha256[:12]}"


async def remote_sha256(sandbox: SandboxLike, remote: str) -> str | None:
    """The SHA-256 of a file in the sandbox, or None when the sandbox cannot hash it, for example because it is missing."""
    result = await sandbox.exec(f"sha256sum {shlex.quote(remote)}", user="root", timeout_sec=COMMAND_TIMEOUT_SECONDS)
    if result.return_code != 0 or not result.stdout or not result.stdout.split():
        return None
    return result.stdout.split()[0]


async def put_file(sandbox: SandboxLike, local: LocalFile, remote: str, *, mode: str, staged_dir: str | None = None) -> None:
    """Make the file at remote an exact copy of local, with the file mode (an octal string such as "755").
    Raises RuntimeError when it cannot be done."""
    if staged_dir is not None:
        source = f"{staged_dir.rstrip('/')}/{local.staged_name}"
        copied = await sandbox.exec(f"cp {shlex.quote(source)} {shlex.quote(remote)}", user="root", timeout_sec=COMMAND_TIMEOUT_SECONDS)
        if copied.return_code != 0:
            raise RuntimeError(f"Cannot copy the staged file {source}: {(copied.stderr or '').strip()}")
        found = await remote_sha256(sandbox, remote)
        if found != local.sha256:
            raise RuntimeError(f"The staged file {source} is not the file of this run: expected {local.sha256}, found {found}")
    else:
        for attempt in range(1, UPLOAD_ATTEMPTS + 1):
            await sandbox.upload_file(local.path, remote)
            found = await remote_sha256(sandbox, remote)
            if found == local.sha256:
                break
            if attempt == UPLOAD_ATTEMPTS:
                raise RuntimeError(f"{local.path.name} arrived damaged {UPLOAD_ATTEMPTS} times: expected {local.sha256}, found {found}")
    changed = await sandbox.exec(f"chmod {mode} {shlex.quote(remote)}", user="root", timeout_sec=COMMAND_TIMEOUT_SECONDS)
    if changed.return_code != 0:
        raise RuntimeError(f"Cannot set the mode of {remote}: {(changed.stderr or '').strip()}")
