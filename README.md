# Inkling

Go coding agent for `thinkingmachines/inkling-small` through OpenRouter. Standard library only; macOS and Linux.

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

Includes file tools, Bash, saved chat, JSON events, RPC, and ACP.

File changes, commands, and web requests require approval. `-yes` disables it. Commands are not sandboxed. `OPENROUTER_API_KEY` overrides the stored key.

`web_search` sends queries to [Exa's free, rate-limited service](https://exa.ai/docs/get-started/exa-mcp); no extra key is required. `web_fetch` reads source URLs.

Chat keeps terminal scrollback. Ctrl-J adds a line. Up/Down moves within a draft; Ctrl-P/Ctrl-N selects history. Paste stays editable. Tool previews mark success or failure. `/tools` shows full results; `/help` lists keys; `/status` shows session details.

## Context and images

Attach with `-file path` or `@path`. Images stay in private session state: 5 MiB each. `inspect_images` sends up to eight images (10 MiB total) per request. Later turns reuse notes; re-inspection uses the original even after source deletion.

Use `context_checkpoint` for working notes and `context_read` for exact older messages. Notes are lossy, not a privacy filter. Back up the session `resources` directory; chat exports exclude resources.

## Develop

Requires Go 1.24 or later, Git, and Bash.

```sh
go test -race ./...
go install ./cmd/think
```

## Evaluate

Tasks for `think eval`: `evals/tasks.jsonl` and `evals/harder.jsonl`. Replies do not prove success.

Terminal-Bench 2.0 supports seven native harnesses on 89 tasks in VMs, including OpenCode and Goose. Install Harbor 0.21.0 and Modal 1.6.1, configure Modal authentication, and set `OPENROUTER_API_KEY`. From the repository root:

```sh
mkdir -p out/terminal-bench &&
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o out/terminal-bench/think-linux-amd64 ./cmd/think &&
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o out/terminal-bench/harbor-wire-linux-amd64 ./evals/harbor-wire &&
python -m evals.harbor_run evals/terminal-bench.json &&
python -m evals.harbor_wire out/terminal-bench/jobs/terminal-bench-2.0-native
```

`compact_tokens` sets Think's threshold. Messages/Responses routes must be verified. No universal lead is established.
