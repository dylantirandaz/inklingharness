# Inkling

Go coding agent for `thinkingmachines/inkling-small` through OpenRouter. It uses the Go standard library and supports macOS and Linux.

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

Streamed chat, saved sessions, file tools, Bash, JSON events, RPC, and ACP are supported.

File changes and commands require approval. `-yes` disables it. Commands are not sandboxed. `OPENROUTER_API_KEY` overrides the stored key.

Chat keeps terminal scrollback. Ctrl-J adds a line. Up/Down moves within a draft; Ctrl-P/Ctrl-N selects history. Paste stays editable. Tool previews mark success or failure. `/tools` shows full results; `/help` lists keys; `/status` shows session details.

## Context and images

Attach with `-file path` or `@path`. Images are stored once in private session state: 5 MiB each. `inspect_images` sends up to eight images (10 MiB total) in a separate request. Later turns reuse notes. Re-inspection reads the stored original, even after source deletion.

The agent can use `context_checkpoint` to retain notes and a next step. `context_read` retrieves exact older messages. Notes are lossy, not a privacy filter. Keep the session state's `resources` directory with session backups. Chat exports do not include stored resources.

## Develop

Requires Go 1.24 or later, Git, and Bash.

```sh
go test -race ./...
go install ./cmd/think
```

## Evaluate

Tasks for `think eval`: `evals/tasks.jsonl` and `evals/harder.jsonl`. Replies do not prove success.

Terminal-Bench 2.0 compares five harnesses on 89 tasks in VMs. Install Harbor 0.21.0 and Modal 1.6.1, configure Modal authentication, and set `OPENROUTER_API_KEY`. From the repository root:

```sh
mkdir -p out/terminal-bench &&
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o out/terminal-bench/think-linux-amd64 ./cmd/think &&
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o out/terminal-bench/harbor-wire-linux-amd64 ./evals/harbor-wire &&
python -m evals.harbor_run evals/terminal-bench.json &&
python -m evals.harbor_wire out/terminal-bench/jobs/terminal-bench-2.0-566b971-vm
```

The recorder checks final Messages and Responses routing metadata. Unverified routes fail. No universal lead is established.
