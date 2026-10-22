"""Put the files that agents install onto a Modal volume, so that sandboxes copy them and nothing is uploaded.

Each file goes on the volume under the name <file name>-<first 12 digits of its SHA-256>
(evals.harbor_binary.LocalFile.staged_name). A name with the hash in it never holds two different
files, so a volume can hold the files of every build and a run names the build by its digest.
A file that is already on the volume is not uploaded again.

After the upload, the tool starts a small sandbox that mounts the volume read-only, as the trials do,
and compares the SHA-256 of every staged file in the sandbox with the local one. It exits 1 when any
file is missing or differs.

  python -m evals.stage_agent_files --volume inkling-agent-binaries \\
      out/terminal-bench/think-noweb-linux-amd64 out/terminal-bench/bare-logged-linux-amd64 \\
      "$(python -c 'import certifi; print(certifi.where())')"

A job config then mounts the volume read-only (environment kwargs {"volumes": {"/agent-binaries":
"inkling-agent-binaries"}}) and gives the agents "staged_dir": "/agent-binaries".
"""

import argparse
from pathlib import Path
import sys

from evals.harbor_binary import LocalFile

MOUNT = "/staged"
APP = "inkling-terminal-bench"


def missing_from(volume_names: set[str], files: list[LocalFile]) -> list[LocalFile]:
    """The files whose staged name is not on the volume yet."""
    return [file for file in files if file.staged_name not in volume_names]


def hash_mismatches(expected: dict[str, str], reported: dict[str, str]) -> dict[str, tuple[str, str | None]]:
    """The staged names whose hash in the sandbox is not the expected one, with both. A name that the sandbox
    did not report is a mismatch with None."""
    return {name: (digest, reported.get(name)) for name, digest in expected.items() if reported.get(name) != digest}


def main() -> int:
    import modal  # Imported here so that the pure functions above work without Modal.

    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--volume", required=True, help="the name of the Modal volume; it is created when it does not exist")
    parser.add_argument("files", nargs="+", type=Path)
    arguments = parser.parse_args()
    files = [LocalFile.read(path) for path in arguments.files]
    names = [file.staged_name for file in files]
    if len(set(names)) != len(names):
        parser.error("Two files have the same staged name")

    volume = modal.Volume.from_name(arguments.volume, create_if_missing=True)
    present = {Path(entry.path).name for entry in volume.listdir("/")}
    to_upload = missing_from(present, files)
    if to_upload:
        with volume.batch_upload() as batch:
            for file in to_upload:
                batch.put_file(file.path, f"/{file.staged_name}")
    print(f"uploaded {len(to_upload)} of {len(files)} files; the other {len(files) - len(to_upload)} were already on the volume")

    app = modal.App.lookup(APP, create_if_missing=True)
    sandbox = modal.Sandbox.create(app=app, image=modal.Image.debian_slim(), volumes={MOUNT: volume.read_only()}, timeout=300)
    try:
        process = sandbox.exec("bash", "-c", f"cd {MOUNT} && sha256sum {' '.join(names)}")
        output = process.stdout.read()
        if process.wait() != 0:
            print(f"sha256sum failed in the sandbox: {process.stderr.read().strip()}", file=sys.stderr)
            return 1
    finally:
        sandbox.terminate()
    reported = {line.split()[1]: line.split()[0] for line in output.splitlines() if len(line.split()) == 2}
    mismatches = hash_mismatches({file.staged_name: file.sha256 for file in files}, reported)
    for file in files:
        print(f"{'MISMATCH' if file.staged_name in mismatches else 'verified':<9} {file.sha256}  {MOUNT}/{file.staged_name}")
    return 1 if mismatches else 0


if __name__ == "__main__":
    raise SystemExit(main())
