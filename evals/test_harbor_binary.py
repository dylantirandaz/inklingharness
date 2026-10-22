"""Checks that a file reaches a sandbox whole, by upload or from a staged volume."""

import hashlib
from pathlib import Path
import shutil
import tempfile
import unittest

from evals.harbor_binary import UPLOAD_ATTEMPTS, LocalFile, put_file, remote_sha256


class Result:
    def __init__(self, return_code: int = 0, stdout: str | None = "", stderr: str | None = "") -> None:
        self.return_code, self.stdout, self.stderr = return_code, stdout, stderr


class FakeSandbox:
    """A sandbox whose files are a dict. damaged_uploads is how many uploads arrive with a flipped byte."""

    def __init__(self, *, damaged_uploads: int = 0, staged: dict[str, bytes] | None = None) -> None:
        self.files: dict[str, bytes] = dict(staged or {})
        self.modes: dict[str, str] = {}
        self.uploads = 0
        self.damaged_uploads = damaged_uploads

    async def upload_file(self, source_path: Path | str, target_path: str) -> None:
        data = Path(source_path).read_bytes()
        self.uploads += 1
        if self.uploads <= self.damaged_uploads:
            data = bytes([data[0] ^ 0xFF]) + data[1:]
        self.files[target_path] = data

    async def exec(self, command: str, cwd=None, env=None, timeout_sec=None, user=None) -> Result:
        parts = command.replace("'", "").split()
        match parts[0]:
            case "sha256sum":
                if parts[1] not in self.files:
                    return Result(1, "", f"sha256sum: {parts[1]}: No such file or directory")
                return Result(0, f"{hashlib.sha256(self.files[parts[1]]).hexdigest()}  {parts[1]}\n")
            case "cp":
                if parts[1] not in self.files:
                    return Result(1, "", f"cp: cannot stat '{parts[1]}': No such file or directory")
                self.files[parts[2]] = self.files[parts[1]]
                return Result(0)
            case "chmod":
                self.modes[parts[2]] = parts[1]
                return Result(0)
            case _:
                raise AssertionError(command)


class PutFileTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self) -> None:
        directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, directory, ignore_errors=True)
        self.path = directory / "think-linux-amd64"
        self.path.write_bytes(b"\x7fELF" + bytes(range(256)) * 40)
        self.local = LocalFile.read(self.path)

    async def test_an_upload_that_arrives_whole_is_installed_with_its_mode_after_one_upload(self) -> None:
        sandbox = FakeSandbox()
        await put_file(sandbox, self.local, "/installed-agent/think", mode="755")
        self.assertEqual(sandbox.files["/installed-agent/think"], self.path.read_bytes())
        self.assertEqual((sandbox.uploads, sandbox.modes["/installed-agent/think"]), (1, "755"))

    async def test_a_damaged_upload_is_uploaded_again_and_the_file_that_stays_is_the_whole_one(self) -> None:
        sandbox = FakeSandbox(damaged_uploads=2)
        await put_file(sandbox, self.local, "/installed-agent/think", mode="755")
        self.assertEqual(sandbox.uploads, 3)
        self.assertEqual(sandbox.files["/installed-agent/think"], self.path.read_bytes())

    async def test_an_upload_that_is_damaged_every_time_is_an_error_after_the_last_attempt_and_no_mode_is_set(self) -> None:
        sandbox = FakeSandbox(damaged_uploads=99)
        with self.assertRaisesRegex(RuntimeError, f"arrived damaged {UPLOAD_ATTEMPTS} times"):
            await put_file(sandbox, self.local, "/installed-agent/think", mode="755")
        self.assertEqual(sandbox.uploads, UPLOAD_ATTEMPTS)
        self.assertEqual(sandbox.modes, {})

    async def test_a_staged_file_is_copied_by_its_hash_name_and_nothing_is_uploaded(self) -> None:
        staged = {f"/agent-binaries/{self.local.staged_name}": self.path.read_bytes()}
        sandbox = FakeSandbox(staged=staged)
        await put_file(sandbox, self.local, "/installed-agent/think", mode="755", staged_dir="/agent-binaries/")
        self.assertEqual(sandbox.files["/installed-agent/think"], self.path.read_bytes())
        self.assertEqual(sandbox.uploads, 0)

    async def test_a_staged_file_that_is_missing_is_an_error_and_is_not_replaced_by_an_upload(self) -> None:
        sandbox = FakeSandbox()
        with self.assertRaisesRegex(RuntimeError, "Cannot copy the staged file"):
            await put_file(sandbox, self.local, "/installed-agent/think", mode="755", staged_dir="/agent-binaries")
        self.assertEqual(sandbox.uploads, 0)

    async def test_a_staged_file_with_the_right_name_and_the_wrong_content_is_an_error(self) -> None:
        wrong = {f"/agent-binaries/{self.local.staged_name}": b"another build"}
        with self.assertRaisesRegex(RuntimeError, "is not the file of this run"):
            await put_file(FakeSandbox(staged=wrong), self.local, "/installed-agent/think", mode="755", staged_dir="/agent-binaries")


class LocalFileTests(unittest.IsolatedAsyncioTestCase):
    def test_the_staged_name_holds_the_file_name_and_the_first_twelve_digits_of_the_hash(self) -> None:
        directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, directory, ignore_errors=True)
        (directory / "bare-linux").write_bytes(b"abc")
        local = LocalFile.read(directory / "bare-linux")
        self.assertEqual(local.sha256, hashlib.sha256(b"abc").hexdigest())
        self.assertEqual(local.staged_name, "bare-linux-ba7816bf8f01")

    async def test_a_sandbox_that_cannot_hash_a_file_gives_none(self) -> None:
        self.assertIsNone(await remote_sha256(FakeSandbox(), "/missing"))


if __name__ == "__main__":
    unittest.main()
