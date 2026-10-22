"""Checks that closing a task limits the agent phase and nothing else."""

from pathlib import Path
import shutil
import tempfile
import unittest

from harbor.models.task.config import NetworkMode, TaskConfig

from evals.closed_tasks import close_agent_network, copy_closed, find_tasks, put_testbed_first, remove_package_caches

# The shape of a SWE-bench task: the public network, and an agent table that holds only a timeout.
TASK = """schema_version = "1.1"

[task]
name = "swe-bench/example__example-1"

[agent]
timeout_sec = 3000.0

[environment]
cpus = 1
allow_internet = true
"""


class CloseAgentNetworkTests(unittest.TestCase):
    def test_the_agent_phase_gets_the_allowlist_and_keeps_its_other_settings(self) -> None:
        config = TaskConfig.model_validate_toml(close_agent_network(TASK, ["openrouter.ai"]))
        policy = config.agent.explicit_phase_policy()
        self.assertIsNotNone(policy)
        self.assertEqual(policy.network_mode, NetworkMode.ALLOWLIST)
        self.assertEqual(policy.allowed_hosts, ["openrouter.ai"])
        self.assertEqual(config.agent.timeout_sec, 3000.0)

    def test_the_environment_baseline_stays_public_so_a_harness_can_install_itself(self) -> None:
        config = TaskConfig.model_validate_toml(close_agent_network(TASK, ["openrouter.ai"]))
        self.assertEqual(config.environment.resolve_baseline().network_mode, NetworkMode.PUBLIC)

    def test_a_task_without_an_agent_table_gets_one(self) -> None:
        without_table = TASK.replace('[agent]\ntimeout_sec = 3000.0\n\n', "")
        config = TaskConfig.model_validate_toml(close_agent_network(without_table, ["openrouter.ai"]))
        self.assertEqual(config.agent.explicit_phase_policy().allowed_hosts, ["openrouter.ai"])

    def test_a_task_that_already_has_an_agent_policy_is_refused(self) -> None:
        closed = close_agent_network(TASK, ["openrouter.ai"])
        with self.assertRaisesRegex(ValueError, "already sets"):
            close_agent_network(closed, ["example.com"])

    def test_an_empty_allowlist_is_refused_because_the_agent_could_not_reach_the_model(self) -> None:
        with self.assertRaises(ValueError):
            close_agent_network(TASK, [])



class PutTestbedFirstTests(unittest.TestCase):
    DOCKERFILE = "FROM swebench/example:latest\nWORKDIR /testbed\nRUN mkdir -p /logs\n"

    def test_the_testbed_environment_comes_first_on_the_path_and_the_old_path_follows(self) -> None:
        line = put_testbed_first(self.DOCKERFILE).splitlines()[-1]
        self.assertTrue(line.startswith("ENV PATH=/opt/miniconda3/envs/testbed/bin:/opt/miniconda3/bin:"))

    def test_the_rest_of_the_dockerfile_is_unchanged(self) -> None:
        self.assertTrue(put_testbed_first(self.DOCKERFILE).startswith(self.DOCKERFILE))

    def test_a_dockerfile_that_sets_the_path_is_refused_rather_than_overridden(self) -> None:
        with self.assertRaisesRegex(ValueError, "already sets PATH"):
            put_testbed_first(self.DOCKERFILE + "ENV PATH=/custom/bin:$PATH\n")


class RemovePackageCachesTests(unittest.TestCase):
    DOCKERFILE = "FROM swebench/example:latest\nRUN mkdir -p /logs\n"

    def test_the_last_step_deletes_the_conda_and_pip_caches_and_the_rest_is_unchanged(self) -> None:
        changed = remove_package_caches(self.DOCKERFILE)
        self.assertTrue(changed.startswith(self.DOCKERFILE))
        self.assertEqual(changed.splitlines()[-1], "RUN rm -rf /opt/miniconda3/pkgs /root/.cache/pip")


def write_task(folder: Path, dockerfile: str = "FROM example\n") -> None:
    (folder / "environment").mkdir(parents=True)
    (folder / "task.toml").write_text(TASK)
    (folder / "environment" / "Dockerfile").write_text(dockerfile)
    (folder / "instruction.md").write_text("Do the thing.\n")


class FindTasksTests(unittest.TestCase):
    def test_a_package_layout_names_a_task_by_the_folder_above_its_digest(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            write_task(Path(root) / "django__django-1" / "digest-a")
            self.assertEqual(find_tasks(Path(root), "packages"), {"django__django-1": Path(root) / "django__django-1" / "digest-a"})

    def test_a_git_layout_names_a_task_by_its_own_folder(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            write_task(Path(root) / "opaque-id" / "fix-git")
            self.assertEqual(find_tasks(Path(root), "git"), {"fix-git": Path(root) / "opaque-id" / "fix-git"})

    def test_two_cached_versions_of_one_task_are_refused_rather_than_guessed(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            write_task(Path(root) / "a" / "fix-git")
            write_task(Path(root) / "b" / "fix-git")
            with self.assertRaisesRegex(ValueError, "Two cached versions"):
                find_tasks(Path(root), "git")


class CopyClosedTests(unittest.TestCase):
    def copy(self, swe_image: bool) -> Path:
        root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, root, ignore_errors=True)
        write_task(root / "cache" / "id" / "fix-git")
        tasks = find_tasks(root / "cache", "git")
        copy_closed(tasks, ["fix-git"], root / "closed", ["openrouter.ai"], swe_image=swe_image)
        return root / "closed" / "fix-git"

    def test_the_copy_is_closed_and_keeps_every_other_file(self) -> None:
        copy = self.copy(swe_image=False)
        config = TaskConfig.model_validate_toml((copy / "task.toml").read_text())
        self.assertEqual(config.agent.explicit_phase_policy().allowed_hosts, ["openrouter.ai"])
        self.assertEqual((copy / "instruction.md").read_text(), "Do the thing.\n")
        self.assertEqual((copy / "environment" / "Dockerfile").read_text(), "FROM example\n")

    def test_the_swe_image_fixes_are_added_only_when_asked_for(self) -> None:
        dockerfile = (self.copy(swe_image=True) / "environment" / "Dockerfile").read_text()
        self.assertIn("ENV PATH=/opt/miniconda3/envs/testbed/bin", dockerfile)
        self.assertIn("RUN rm -rf /opt/miniconda3/pkgs", dockerfile)

    def test_a_name_without_a_cached_task_is_refused(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            with self.assertRaisesRegex(ValueError, "No cached task"):
                copy_closed({}, ["fix-git"], Path(root) / "closed", ["openrouter.ai"], swe_image=False)


if __name__ == "__main__":
    unittest.main()
