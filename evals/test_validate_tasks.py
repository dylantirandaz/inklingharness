"""Checks the rule that decides which tasks an airgapped run may use."""

import unittest

from evals.validate_tasks import Probe, parse_probe, reasons_to_exclude, validate

CLEAN = Probe(complete=True, outside_commits_not_older_than_base=0, package_dirs=(), archives=())

OUTPUT = """task=django__django-11433
base_commit_date=1553101200
refs=12
outside_commits=3
outside_commits_not_older_than_base=0
conda_pkgs_entries=0
pip_cache_kb=
done=1
"""


class ParseProbeTests(unittest.TestCase):
    def test_a_clean_output_gives_the_task_and_no_findings(self) -> None:
        task, probe = parse_probe(OUTPUT)
        self.assertEqual(task, "django__django-11433")
        self.assertEqual(probe, CLEAN)

    def test_copies_and_archives_are_collected_and_an_output_that_stops_early_is_incomplete(self) -> None:
        task, probe = parse_probe("task=pydata__xarray-4966\nouter=1\npackage_dir=/opt/x/xarray\narchive=/opt/x/xarray-2025.tar.gz\nouter_commits=1\noutside_commits_not_older_than_base=2\n")
        self.assertEqual(task, "pydata__xarray-4966")
        self.assertEqual((probe.package_dirs, probe.archives, probe.complete, probe.outside_commits_not_older_than_base), (("/opt/x/xarray",), ("/opt/x/xarray-2025.tar.gz",), False, 2))

    def test_an_output_without_a_task_is_refused(self) -> None:
        with self.assertRaises(ValueError):
            parse_probe("done=1\n")


class ReasonsTests(unittest.TestCase):
    def test_a_task_that_passes_both_checks_is_valid(self) -> None:
        self.assertEqual(reasons_to_exclude(True, CLEAN), ())

    def test_a_failed_oracle_excludes_the_task(self) -> None:
        self.assertEqual(reasons_to_exclude(False, CLEAN), ("oracle_failed",))

    def test_a_check_that_never_ran_excludes_the_task_and_is_not_taken_as_a_pass(self) -> None:
        self.assertEqual(reasons_to_exclude(None, None), ("oracle_missing", "probe_missing"))
        stopped = Probe(complete=False, outside_commits_not_older_than_base=0, package_dirs=(), archives=())
        self.assertEqual(reasons_to_exclude(True, stopped), ("probe_incomplete",))
        unknown = Probe(complete=True, outside_commits_not_older_than_base=None, package_dirs=(), archives=())
        self.assertEqual(reasons_to_exclude(True, unknown), ("probe_incomplete",))

    def test_every_finding_of_the_probe_excludes_the_task_and_each_one_is_named(self) -> None:
        found = Probe(complete=True, outside_commits_not_older_than_base=2, package_dirs=("/opt/pkgs/xarray",), archives=("/opt/pkgs/xarray.conda",))
        self.assertEqual(reasons_to_exclude(True, found), ("package_copy_outside_testbed", "archive_of_the_package", "newer_commits_outside_head"))

    def test_commits_outside_head_that_are_older_than_the_base_do_not_exclude_the_task(self) -> None:
        # A release tag from before the issue, as in matplotlib-22719, cannot hold the fix.
        self.assertEqual(reasons_to_exclude(True, CLEAN), ())


class ValidateTests(unittest.TestCase):
    def test_the_valid_tasks_keep_the_order_and_every_exclusion_has_its_reasons(self) -> None:
        order = ["a", "b", "c", "d"]
        oracle = {"a": True, "b": False, "c": True}
        probes = {"a": CLEAN, "b": CLEAN, "c": Probe(True, 0, ("/x/c",), ())}
        result = validate(order, oracle, probes)
        self.assertEqual(result.valid, ("a",))
        self.assertEqual(result.excluded, {"b": ("oracle_failed",), "c": ("package_copy_outside_testbed",), "d": ("oracle_missing", "probe_missing")})


if __name__ == "__main__":
    unittest.main()
