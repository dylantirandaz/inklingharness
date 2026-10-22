"""Checks the decisions of the staging tool."""

from pathlib import Path
import unittest

from evals.harbor_binary import LocalFile
from evals.stage_agent_files import hash_mismatches, missing_from

A = LocalFile(Path("/x/think-linux"), "a" * 64)
B = LocalFile(Path("/x/bare-linux"), "b" * 64)


class MissingFromTests(unittest.TestCase):
    def test_a_file_whose_staged_name_is_on_the_volume_is_not_uploaded_again(self) -> None:
        self.assertEqual(missing_from({A.staged_name}, [A, B]), [B])

    def test_a_file_with_the_same_name_and_another_content_is_a_different_staged_name_and_is_uploaded(self) -> None:
        changed = LocalFile(Path("/x/think-linux"), "c" * 64)
        self.assertEqual(missing_from({A.staged_name}, [changed]), [changed])


class HashMismatchTests(unittest.TestCase):
    def test_equal_hashes_give_no_mismatch(self) -> None:
        self.assertEqual(hash_mismatches({A.staged_name: A.sha256}, {A.staged_name: A.sha256}), {})

    def test_a_different_hash_and_a_name_that_the_sandbox_did_not_report_are_both_mismatches(self) -> None:
        expected = {A.staged_name: A.sha256, B.staged_name: B.sha256}
        reported = {A.staged_name: "d" * 64}
        self.assertEqual(hash_mismatches(expected, reported), {A.staged_name: (A.sha256, "d" * 64), B.staged_name: (B.sha256, None)})


if __name__ == "__main__":
    unittest.main()
