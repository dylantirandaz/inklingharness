"""Checks that Pi's option parsing ends before the task instruction."""

from pathlib import Path
import tempfile
import unittest

from harbor.agents.installed.pi import Pi

from evals.harbor_pi import PiWithSafeInstruction

MODEL = "openrouter/thinkingmachines/inkling-small"


def flags(agent_class: type[Pi], **options: str) -> str:
    with tempfile.TemporaryDirectory() as logs:
        return agent_class(logs_dir=Path(logs), model_name=MODEL, version="0.87.1", **options).build_cli_flags()


class PiWithSafeInstructionTests(unittest.TestCase):
    def test_the_flags_of_harbor_stay_and_end_with_the_option_terminator(self) -> None:
        # Harbor puts the instruction right after the flags, so Pi reads it as a message even when it starts with "-".
        self.assertEqual(flags(Pi, thinking="high"), "--thinking high")
        self.assertEqual(flags(PiWithSafeInstruction, thinking="high"), "--thinking high --")

    def test_without_other_flags_the_terminator_stands_alone(self) -> None:
        self.assertEqual(flags(PiWithSafeInstruction), "--")

    def test_the_trial_still_reports_the_harness_name_pi(self) -> None:
        with tempfile.TemporaryDirectory() as logs:
            agent = PiWithSafeInstruction(logs_dir=Path(logs), model_name=MODEL, version="0.87.1")
            self.assertEqual(agent.to_agent_info().name, "pi")


if __name__ == "__main__":
    unittest.main()
