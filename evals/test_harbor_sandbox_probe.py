"""Checks how the sandbox probe finds its task and its packages."""

from pathlib import Path
import shutil
import tempfile
import unittest

from evals.harbor_sandbox_probe import PACKAGES, instance_of, packages_of


class PackagesTests(unittest.TestCase):
    def test_every_repository_of_swe_bench_verified_has_its_packages(self) -> None:
        owners = {"astropy", "django", "matplotlib", "mwaskom", "pallets", "psf", "pydata", "pylint-dev", "pytest-dev", "scikit-learn", "sphinx-doc", "sympy"}
        self.assertEqual(set(PACKAGES), owners)

    def test_the_package_comes_from_the_owner_in_the_instance_name(self) -> None:
        self.assertEqual(packages_of("scikit-learn__scikit-learn-25232"), ("sklearn",))
        self.assertEqual(packages_of("pytest-dev__pytest-7324"), ("_pytest", "pytest"))

    def test_an_unknown_owner_is_an_error_and_not_an_empty_search_that_would_pass_every_task(self) -> None:
        with self.assertRaisesRegex(ValueError, "No package is known"):
            packages_of("someone__newrepo-1")


class InstanceTests(unittest.TestCase):
    def folder(self) -> Path:
        directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, directory, ignore_errors=True)
        for name, text in {"a__a-1": "Fix the first\nbug.", "b__b-2": "Fix the second bug."}.items():
            (directory / name).mkdir()
            (directory / name / "instruction.md").write_text(text)
        return directory

    def test_the_instance_is_found_by_the_text_of_its_instruction_whatever_the_spacing(self) -> None:
        self.assertEqual(instance_of("Fix  the first bug.  ", self.folder()), "a__a-1")

    def test_a_text_that_matches_no_task_or_two_tasks_is_an_error(self) -> None:
        directory = self.folder()
        with self.assertRaises(ValueError):
            instance_of("Another text", directory)
        (directory / "c__c-3").mkdir()
        (directory / "c__c-3" / "instruction.md").write_text("Fix the second bug.")
        with self.assertRaises(ValueError):
            instance_of("Fix the second bug.", directory)


if __name__ == "__main__":
    unittest.main()
