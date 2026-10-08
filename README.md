# Inkling

A small Go coding agent for `thinkingmachines/inkling-small` through OpenRouter. It uses only the Go standard library and supports macOS and Linux.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/dylantirandaz/inklingharness/main/install.sh | sh
think login
think
```

OpenRouter credits are required. Prompts and tool results go to the provider; do not send private code without checking its data policy.

## Use

```sh
think run "Find and fix the failing test"
think chat -resume last
think run -h
```

The agent provides streamed chat, saved sessions, file reads, search, exact text edits, file writes, and Bash commands. JSON events, RPC, and ACP support use from other programs.

File changes and commands require approval unless you enable `-yes`. Commands run on your machine; this is not a sandbox. `OPENROUTER_API_KEY` overrides the stored key. Use `-h` on each command for its options.

## Context and images

Attach files with `-file path` or `@path`. Images are stored once in private session state, with a 5 MiB limit each. `inspect_images` sends selected images in a separate request; later turns reuse text notes. Each call accepts up to eight images and 10 MiB. Explicit re-inspection uses the saved original, even after source deletion.

The agent can use `context_checkpoint` to retain notes and a next step. `context_read` retrieves exact older messages. Notes are lossy, not a privacy filter. Keep the session state's `resources` directory with session backups. Chat exports do not include stored resources.

## Develop

Requires Go 1.24 or later, Git, and Bash.

```sh
go test -race ./...
go install ./cmd/think
```

## Evaluate

`evals/tasks.jsonl` has 20 basic tasks. `evals/harder.jsonl` adds six cases for cancellation, parsing, API changes, atomic updates, intervals, and report selection. Each task has a separate check; a final model reply alone is not a pass.

```sh
think eval -yes -model thinkingmachines/inkling-small -effort high \
  -max-turns 20 \
  -extra '{"provider":{"order":["deepinfra/fp8"],"allow_fallbacks":false}}' \
  evals/harder.jsonl
```

Local allocation checks improved. Total model cost and time still vary by task. Current results do not establish a lead over every other harness.
