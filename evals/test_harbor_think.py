"""Checks how the Think adapter treats a system prompt file."""

from pathlib import Path
import shutil
import tempfile
import unittest

from evals.harbor_binary import LocalFile
from evals.harbor_think import Think

MODEL = "openrouter/thinkingmachines/inkling-small"


class SystemFileTests(unittest.TestCase):
    def setUp(self) -> None:
        self.directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.directory, ignore_errors=True)
        self.binary = self.directory / "think"
        self.binary.write_bytes(b"\x7fELF\x02\x01" + bytes(12) + b"\x3e\x00" + bytes(40))

    def agent(self, **kwargs) -> Think:
        logs = self.directory / "logs"
        logs.mkdir(exist_ok=True)
        return Think(logs_dir=logs, model_name=MODEL, binary_path=str(self.binary), **kwargs)

    def test_without_a_system_file_the_version_is_the_hash_of_the_binary_alone(self) -> None:
        agent = self.agent()
        self.assertEqual(agent.version(), "sha256:" + LocalFile.read(self.binary).sha256)

    def test_with_a_system_file_its_hash_is_part_of_the_version_so_that_two_prompts_never_share_a_version(self) -> None:
        first, second = self.directory / "a.txt", self.directory / "b.txt"
        first.write_text("Be brief.\n")
        second.write_text("Be thorough.\n")
        versions = {self.agent(system_file=str(path)).version() for path in (first, second)}
        self.assertEqual(len(versions), 2)
        for version in versions:
            self.assertTrue(version.startswith("sha256:" + LocalFile.read(self.binary).sha256 + "+system:"))

    def test_a_system_file_that_does_not_exist_is_an_error_at_construction_and_not_in_the_sandbox(self) -> None:
        with self.assertRaises(FileNotFoundError):
            self.agent(system_file=str(self.directory / "missing.txt"))


if __name__ == "__main__":
    unittest.main()
