"""Checks that the native Modal environment mounts volumes read-only."""

import unittest
from unittest import mock

from evals.harbor_environment import NativeModalEnvironment


class FakeVolume:
    def __init__(self, name: str, read_only: bool = False) -> None:
        self.name, self.is_read_only = name, read_only

    def read_only(self) -> "FakeVolume":
        return FakeVolume(self.name, True)


class VolumesConfigTests(unittest.TestCase):
    def environment(self, volumes: dict[str, str]) -> NativeModalEnvironment:
        # The base class needs a task and a Modal account to build. Only the volume mapping matters here.
        environment = object.__new__(NativeModalEnvironment)
        environment._volumes = volumes
        return environment

    def test_every_volume_is_mounted_read_only_at_its_path(self) -> None:
        with mock.patch("harbor.environments.modal.Volume", create=True) as volume_class:
            volume_class.from_name.side_effect = FakeVolume
            mounted = self.environment({"/agent-binaries": "inkling-agent-binaries", "/data": "other"})._volumes_config()
        self.assertEqual({path: (volume.name, volume.is_read_only) for path, volume in mounted.items()}, {"/agent-binaries": ("inkling-agent-binaries", True), "/data": ("other", True)})

    def test_no_volumes_mean_no_mounts(self) -> None:
        with mock.patch("harbor.environments.modal.Volume", create=True):
            self.assertEqual(self.environment({})._volumes_config(), {})


if __name__ == "__main__":
    unittest.main()
