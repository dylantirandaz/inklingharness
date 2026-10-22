"""Harbor's Pi agent, with the task instruction after `--`.

Harbor puts the instruction at the end of the Pi command as a bare argument. Pi reads
a bare argument that starts with "-" as an option, so an instruction that starts with
"-" fails with "Unknown option" before the agent runs. Pi treats every argument after
`--` as a message, so this change leaves every other instruction as it was.

One task of Terminal-Bench 2.0, pytorch-model-recovery, has such an instruction.
"""

from harbor.agents.installed.pi import Pi


class PiWithSafeInstruction(Pi):
    def build_cli_flags(self) -> str:
        flags = super().build_cli_flags()
        return f"{flags} --" if flags else "--"
