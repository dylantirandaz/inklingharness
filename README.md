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

Chat opens with the artwork of the [Inkling page](https://thinkingmachines.ai/inkling/): an ink blot with three colored shapes. The page draws it from a seed. Chat picks one of the page's seeds from your project path, so each project keeps its own picture. The picture needs 256 colors, 64 columns, and 24 rows. A smaller terminal shows the project name only. The artwork matches the page for each seed.

Chat keeps normal terminal scrollback and a fixed multiline composer. The composer is a soft card with no border. A terminal with fewer than 256 colors keeps the bordered card. Ctrl-J adds a line. Up/Down moves within a draft; Ctrl-P/Ctrl-N selects history. Paste stays editable. Each prompt you send stays as a card with the time at its right edge.

A reply is plain text with one left margin: no marker, inline code and file paths in azure, and thin bullets. A tool, the model, and a turn each have their own look:

- **Tools** are cards. A green tint means the tool worked, and a red tint means that it failed. The first row has the verb, the target, the time, and `#N`, the number of the tool's tab in `/tools N`. A read, a search, and a file listing show a count in that row, such as `15 lines` or `4 matches`, and no output. An edit or a write shows its diff, with the line numbers. Other tools show a few lines of output, and a note says how many lines are hidden.
- **Working** shows on a hairline above the composer: the spinner, the state, and the seconds. The state is waiting for the model, thinking, writing, or running tools.
- **A turn** ends with a hairline that holds its wall time, request count, and token use.

The footer has two rows under the composer. The first row names the folder and the Git branch at the left, and the model and effort at the right. The second row has the keys at the left, and the context gauge and the session tokens at the right. A terminal shorter than 16 rows keeps one row.

Type `/` to open the command menu. Up/Down selects a command, Tab completes it, and Escape closes the menu. Enter runs a command without arguments. For a partial command that takes arguments, Enter completes the name and leaves the input open. An exact command name runs on Enter. Project commands also appear in this menu. On terminals smaller than 20 columns or 8 rows, type commands directly.

Tool numbers continue across model requests for one prompt. `/tools` opens the saved tool results for the last prompt; `/tools N` selects a tab. `/diff` shows current Git changes against HEAD, one file per tab with its added and removed line counts, including new non-ignored files, without changing the index.

Detail views use a separate screen buffer only while open. The tab row names each document; a rule and a scrollbar frame the text on screens at least 40 columns wide. Up/Down or j/k scrolls; PageUp/PageDown moves a page; Home/End or g/G moves to the start or end. Left/Right, Tab, or a digit selects a tab. Escape, q, Enter, or Ctrl-C returns to chat. These views do not animate while idle.

The context gauge is four cells against the compaction threshold, and the session tokens include cache tokens. The context count updates after each model request. Code blocks in a reply sit on a card with the language name. `/help` lists keys; `/usage` shows token counts.

`/status` and `/context` show cards like the model cards of the page. Each card has a waffle of squares: `/status` shows the context against the compaction threshold, and `/context` gives each kind of content its own color.

Use `think chat -show-thinking` to show reasoning. The Go renderer batches partial reasoning on its 33 ms frame timer, including when animations are off. Complete lines and text at reply, tool, status, or finish boundaries print at once.

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

Terminal-Bench 4.0 compares harnesses on one model. Use Harbor 0.24.0 and the fixed dataset digest `sha256:39d9f44b40420cde8fdcc087579c0d72a7e14fa3656d603c3f0d22fb35e27732`. The older Luna configs are in `evals/tb4-luna/`; public Astra results are in `out/reference-tb4/`.

The current comparison uses `evals/tb4-deepseek-flash/`: Think, Pi, Claude Code, Codex 0.151.0, and Hermes on `deepseek/deepseek-v4.1-flash`, at max effort. Inference uses OpenRouter's `streamlake/fp8` route, with provider fallbacks disabled. Harbor runs the task environments on Prime, not Modal. The three GPU exclusions use full package names: `terminal-bench/jax-speedrun-gpu`, `terminal-bench/fp8-rmsnorm-gemm`, and `terminal-bench/math-eval-grader`. This selects 63 of 66 tasks, with three attempts each: 189 trials per harness, 945 total. Each harness has two concurrent trials, for ten total.

New job names end in `-prime-streamlake`. Do not combine these results with older Novita runs or local Docker runs. Keep the frozen binaries in `out/terminal-bench/tb4/` unchanged during the comparison. The Prime environment checks native Linux x86-64, the task's CPU count, and its physical memory before the agent or separate verifier runs. Each VM has a `hardware-*.json` record. The model route, effort, and output cap are recorded in `agent/wire/profile.json`; `python -m evals.harbor_wire JOB_DIRECTORY` audits completed trials.

Run `evals.budget_guard` with the controllers in separate process groups. Pass each PID file, repeat `--job-dir` for each owned job, and set `--max-sandboxes 10`. The guard stops the jobs on a spend limit, low credit, a balance-read failure, or excess owned sandboxes. It checks deletion after shutdown and does not delete another job's sandboxes. Carry prior spend with `--already`; wallet top-ups do not subtract earlier recorded charges. The current $400 budget uses a $395 stop threshold, a $3 credit floor, and 30-second checks. Polling and in-flight calls can add charges after the stop threshold.

Exited controllers count as stopped even before their parent collects their exit status. After a network failure, confirm sandbox cleanup and account access before restarting. Preserve interrupted trial files before Harbor resumes, because it removes incomplete trial directories.

## Tinker

`/tinker MODEL` fine-tunes a model of your choice on your own sessions, for example `/tinker Qwen/Qwen3-30B-A3B-Instruct-2507`. Think saves the sessions of the current directory as traces (the exact history, system prompt and tools the model saw), trains a LoRA adapter on every assistant turn with [Tinker](https://thinkingmachines.ai/tinker/) in the background, and switches the chat to the new checkpoint when it is ready. Each later `/tinker MODEL` trains only on sessions that grew since the last round and resumes from the last training state, so the model keeps improving as you work. `/tinker` shows progress; `/model` switches back.

Think serves `tinker://` checkpoints through Tinker's OpenAI-compatible endpoint, so they work with every tool. The base model needs a tool-calling format (Qwen3, DeepSeek, Kimi, gpt-oss). Training needs `TINKER_API_KEY` and a Python with `tinker` and `tinker-cookbook` (`pip install tinker tinker-cookbook`); set `THINK_TINKER_PYTHON` if it is not `python3`. State, traces and logs are in `~/.local/state/inkling/tinker/`.
