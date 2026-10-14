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

Summary requests disable tool calls.

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

`compact_tokens` sets Think's threshold. Native launches use scripts. Verify Messages/Responses routes. Seven-harness results show no universal lead.

Terminal-Bench 4.0 (66 tasks, part of the Artificial Analysis Coding Agent Index) compares harnesses on one model. `evals/tb4-luna/` holds one job per harness (Think, Pi, Claude Code, Codex 0.151.0, Hermes) on `openai/gpt-6-luna` at max effort: the official board's Codex entry (16.4%) is the anchor. Controls match the public Terminal-Bench 4.0 jobs: dataset digest `sha256:39d9f44b…`, Modal, timeout multiplier 1.0, three attempts per task. Use Harbor 0.24.0, and build both binaries into `out/terminal-bench/tb4/`. Each model's route, effort and output cap live in `PROFILES` in `evals/harbor_wire.py`; harbor-wire enforces them and records them in `profile.json`, and the audit checks each trial against it. Public per-task results for GPT-6 Astra (Codex, Pi, Reason) are in `out/reference-tb4/`. `evals/tb4-deepseek-flash/` holds the same five jobs on `deepseek/deepseek-v4.1-flash` at max effort, the cheaper comparison model: DeepSeek reports 31.2% for it on Terminal-Bench 4.0 in its own harness. DeepSeek's first-party route is excluded by OpenRouter accounts that block providers that train on prompts, so the profile pins DeepInfra fp8, which caches repeated prefixes.

## Tinker

`/tinker MODEL` fine-tunes a model of your choice on your own sessions, for example `/tinker Qwen/Qwen3-30B-A3B-Instruct-2507`. Think saves the sessions of the current directory as traces (the exact history, system prompt and tools the model saw), trains a LoRA adapter on every assistant turn with [Tinker](https://thinkingmachines.ai/tinker/) in the background, and switches the chat to the new checkpoint when it is ready. Each later `/tinker MODEL` trains only on sessions that grew since the last round and resumes from the last training state, so the model keeps improving as you work. `/tinker` shows progress; `/model` switches back.

Think serves `tinker://` checkpoints through Tinker's OpenAI-compatible endpoint, so they work with every tool. The base model needs a tool-calling format (Qwen3, DeepSeek, Kimi, gpt-oss). Training needs `TINKER_API_KEY` and a Python with `tinker` and `tinker-cookbook` (`pip install tinker tinker-cookbook`); set `THINK_TINKER_PYTHON` if it is not `python3`. State, traces and logs are in `~/.local/state/inkling/tinker/`.
