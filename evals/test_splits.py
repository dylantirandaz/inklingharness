"""Checks the split rules and the seal on the final test."""

import json
from pathlib import Path
import shutil
import tempfile
import unittest

from evals.splits import Splits, load, overlaps, ran_on_final_test


def splits(**kwargs: set[str]) -> Splits:
    base = {"dev": {"a"}, "round1": {"b"}, "final_test": {"c", "d"}, "final_test_stress": {"d"}, "round1_stress": set()}
    return Splits(**{key: frozenset(value) for key, value in (base | kwargs).items()})


class OverlapTests(unittest.TestCase):
    def test_disjoint_splits_with_stress_lists_inside_their_splits_have_no_overlap(self) -> None:
        self.assertEqual(overlaps(splits()), {})

    def test_a_task_in_two_splits_is_reported_under_both_names(self) -> None:
        self.assertEqual(overlaps(splits(round1={"b", "c"})), {"round1 and final_test": frozenset({"c"})})
        self.assertEqual(overlaps(splits(dev={"a", "b"})), {"dev and round1": frozenset({"b"})})

    def test_a_stress_task_that_is_not_in_its_split_is_reported(self) -> None:
        self.assertEqual(overlaps(splits(final_test_stress={"z"})), {"final_test_stress outside final_test": frozenset({"z"})})


class SealTests(unittest.TestCase):
    def setUp(self) -> None:
        self.jobs = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.jobs, ignore_errors=True)

    def trial(self, job: str, task: str, harness: str) -> None:
        directory = self.jobs / job / f"{task}__abc{harness}"
        directory.mkdir(parents=True)
        (directory / "result.json").write_text(json.dumps({"task_name": f"swe-bench/{task}", "agent_info": {"name": harness}}))

    def test_an_agent_that_calls_a_model_on_a_final_test_task_breaks_the_seal_and_the_reference_solution_does_not(self) -> None:
        self.trial("oracle-job", "c", "oracle")
        self.trial("probe-job", "c", "moreprobe")
        self.assertEqual(ran_on_final_test(splits(), self.jobs), [])
        self.trial("run-1", "c", "think")
        self.trial("run-1", "other-task", "think")
        self.assertEqual(len(ran_on_final_test(splits(), self.jobs)), 1)

    def test_an_agent_that_the_check_does_not_know_counts_as_one_that_calls_a_model(self) -> None:
        self.trial("run-2", "d", "some-new-harness")
        self.assertEqual(len(ran_on_final_test(splits(), self.jobs)), 1)

    def test_a_trial_in_an_archive_folder_counts_too(self) -> None:
        self.trial("archive/faults/job-a", "c", "pi")
        self.assertEqual(len(ran_on_final_test(splits(), self.jobs)), 1)


class FileTests(unittest.TestCase):
    def test_the_splits_file_loads_and_its_splits_are_disjoint(self) -> None:
        loaded = load()
        self.assertEqual(overlaps(loaded), {})
        self.assertGreater(len(loaded.final_test), 0)


if __name__ == "__main__":
    unittest.main()
