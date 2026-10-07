# inkling

A Go coding-agent harness for `thinkingmachines/inkling-small` on OpenRouter.
It uses the Go standard library, streams replies, and runs local tools.
It has no third-party Go dependencies. Commands run as child processes.

**The default is paid Inkling Small.** OpenRouter credits are required.
The real coding evaluation passed both tasks: file creation and a Go bug fix.
The harness does not change models automatically on failure.

The optional `thinkingmachines/inkling:free` endpoint previously returned
HTTP 403 at `Gate Free Endpoints by Agentic Harness`. A valid key alone did
not grant access. Selecting paid Small is a model change, not a gate bypass.

**Data:** prompts and tool results go to OpenRouter and its selected provider.
Review the provider's data policy before sending private code. If you select
an Inkling free endpoint, it logs prompts and outputs for training; do not
send confidential or personal data to it.

## Start

Requirements: Git and Bash; Go 1.24 or later only to build from source. Login uses `stty`
to hide key input. Chat uses native terminal calls for input mode and size.
The CLI supports macOS and Linux. It is not a Windows implementation.

Install the latest release on macOS or Linux (Apple silicon, Intel, arm64,
x86-64). No Go toolchain is needed. It installs `think` into `~/.local/bin`,
or into `THINK_INSTALL_DIR` if that is set, and checks the SHA-256 checksum:

```sh
curl -fsSL https://raw.githubusercontent.com/dylantirandaz/inklingharness/main/install.sh | sh
```

Run the same command again to update. To build from source instead, use
`go install ./cmd/think`. `scripts/release.sh VERSION` builds all four
platforms and publishes a GitHub release.

```sh
# Paste the key once. Input is hidden on a terminal.
think login

# Open a chat in the current directory.
think

# Resume the latest chat in this directory.
think chat -resume last

# List sessions, resume one by its full ID, or pick one from a list.
think sessions
think chat -resume SESSION_ID
think chat -resume pick

# Run one task. Approve file changes and commands at the terminal.
think run "Find and fix the failing test"
```

The command is `think`; the product, the saved sessions, and the stored key
keep the name Inkling. The installer prints the `PATH` line to add if its
directory is not on `PATH`; `go install` uses `$(go env GOPATH)/bin`.

`OPENROUTER_API_KEY` takes precedence over the stored key. Login stores the
key in `$XDG_CONFIG_HOME/inkling/credentials.json`, or
`~/.config/inkling/credentials.json`, with mode `0600`. The key check confirms
key validity, not access to a particular model.

Use `-h` on each command for its flags. Flags go before a one-shot prompt.
A piped prompt is also accepted by `run`.

## Chat controls

Terminal chat uses the "Ink" look, taken from thinkingmachines.ai: ink on
paper, warm grays, the flat green, blue, and red shapes of the Inkling mark,
small tinted chips, and the square grid of the Inkling model cards. Finished
output stays in ordinary terminal scrollback; only a few live rows at the
bottom change.

- The chat opens with the Inkling mark, drawn in half blocks, beside the
  letterspaced `T H I N K I N G   M A C H I N E S` wordmark, the model name
  with its effort on a chip, and the folder. The mark bleeds in like ink from
  one drop, each new pixel pale and then dry; the green shape, the blue dot,
  and the red pill follow; the wordmark types itself. The composer works at
  once, and any output ends the animation on the final banner.
- Your prompts sit on a faint tinted band that starts with `›`. Each reply
  starts with a `●` in the model color: plum for Inkling-Small, blue for
  Inkling, and green for other models. Streamed text arrives pale and dries
  to ink in about a quarter second.
- While the model works, a grid of 16 squares fires like the experts of a
  mixture-of-experts model: a few light up in the model color at each tick,
  and the ones of the previous tick fade. The status and elapsed seconds
  follow. A running tool shows a drop that fills, `○ ◔ ◑ ◕`; the finished row
  shows a green `●` or a red `✕`, the verb in bold, and the target. Previews
  follow a hairline `│`. When the turn ends, the grid slides into a `●`.
- The footer shows the model, effort, and the context estimate as ten
  squares against the compaction threshold, for example `▪▪▪▪▪▪▪▪▪▪ 61k / 200k`.
  A new estimate counts up and fills the squares one by one. Used squares
  turn amber at 80% and red at the threshold; during a turn they breathe.
- An approval request is a card with an amber bar, the command on a chip,
  and the keys `y`, `a`, and `n` on chips. The bar beside the keys breathes
  until you answer.

Every effect is a function of the time and runs only in the live rows, never
in scrollback. The screen draws at 30 frames per second while a short effect
plays (opening, drying text, hand-off, gauge), at 15 while the grid or the
approval bar moves, and not at all at an idle prompt. Set `INKLING_MOTION=off`
to stop every animation; the screen then wakes once a second during a turn to
count its seconds. Another value of `INKLING_MOTION` is an error. Fades need
256 colors; with fewer colors the grid still moves but nothing fades.

At start, chat asks the terminal for its background color (OSC 11, then
DA1). On a light background the mark's blot and the text are ink; on a dark
one they are paper. Every terminal answers DA1, so the question adds no wait
when the terminal answers; one that answers neither costs at most 200 ms and
is taken as dark. A reply that arrives later is dropped, never typed.

Color depth follows the terminal: 24-bit color when `COLORTERM` is
`truecolor` or `24bit`, the 256-color palette when `TERM` has `256color`, and
16 colors otherwise. The mark and the tinted chips need 256 colors; with 16
colors, keys and code take the accent instead. `NO_COLOR` keeps every glyph:
used squares stay `▪`, empty ones become `·`, and keys show as `[y]`.

The window title shows the state: `○` ready, `●` working, or `◉` approval,
and the folder name. Tab bars, such as the one in cmux, show it. When an
approval is needed, or when a turn that took at least 5 seconds ends, chat
sends a desktop notification (OSC 9) that names the command or the first
line of the reply. iTerm2, Ghostty, WezTerm, kitty, and cmux show it; cmux
also rings the pane. Other terminals ignore it. Exit restores the earlier
window title and cursor shape.

Replies use Markdown styles. Partial lines stay literal until they are
complete. Each frame leaves in one write. A frame that changes more than one
row, or adds scrollback text, is wrapped in synchronized output (DEC mode
2026), so supporting terminals never show a half-drawn frame; a one-row
update, such as a grid frame or a typed key, has no markers. An idle
prompt does not wake the process; the screen sleeps until a key, a resize, or
an animation frame is due.

Measured on an Apple M3 in a real 30×100 PTY that answers OSC 11 and DA1
(medians of 3 alternating runs; CPU of the process, bytes it wrote):

| Scenario | v0.2.0 | Motion on | `INKLING_MOTION=off` |
| --- | --- | --- | --- |
| Idle prompt, 10 s | 1.0 ms, 1.0 KB | 1.6 ms, 1.0 KB | 1.1 ms, 1.0 KB |
| Turn, 5 s of status | 22.7 ms, 9.8 KB | 31.6 ms, 14.9 KB | 2.7 ms, 2.2 KB |
| Streamed reply, 5 s at 40 tokens/s | 55.8 ms, 34.4 KB | 66.0 ms, 62.5 KB | 59.5 ms, 30.6 KB |
| Approval wait, 5 s | 1.0 ms, 0.8 KB | 27.9 ms, 33.7 KB | 1.3 ms, 0.8 KB |

The first composer frame appears 27.2 ms after start (v0.2.0: 32.6 ms;
7 runs); the opening plays under it and ends about 0.6 s later. A terminal
that answers neither OSC 11 nor DA1 adds up to 200 ms.

Tool activity is compact. Successful reads and searches do not dump their
contents into the conversation. `/tools` shows the full stored output from
the latest tool batch, within the tool limits below. Use `/verbose` or start
with `-verbose` to show protocol details and full tool results. Reasoning
text stays hidden unless `-show-thinking` is set.

| Command | Action |
| --- | --- |
| `/help` | Show commands and input controls. |
| `/status` | Show session ID, model, directory, context estimate, and approval mode. |
| `/usage` | Show known input, output, and cache token counts. |
| `/tools` | Show the full stored output of the latest tool batch. |
| `/verbose [on\|off]` | Show or hide per-turn protocol details and full tool results. |
| `/model [ID]` | Show or change the model for later requests. |
| `/effort [LEVEL]` | Show or change reasoning effort. `default` restores the model default. |
| `/compact` | Summarize older messages with the selected model. |
| `/clear` | Start a new session. Keep the old session on disk. |
| `/fork` | Continue in a copy of this session, titled "fork of ...". The original stays saved as it was. |
| `/title [TEXT]` | Show or set the session title. `think sessions` and `-resume pick` show it. |
| `/context` | Show what fills the context: system prompt, tool definitions, and the history by kind, largest first. |
| `/plan TASK` | Investigate with read-only tools and reply with a numbered plan. Changes are refused. |
| `/goal [CMD\|off]` | Run the bash command `CMD` now and after each turn. While it fails, its exit status and the end of its output go to the model as the next prompt, at most 5 times. A failed or cancelled turn ends the goal. |
| `/undo` | Restore the files and the conversation from before the last prompt. |
| `/rewind [N]` | List the prompts of this run, or restore the state from before prompt N. |
| `/NAME [ARGS]` | Run your command from `.inkling/commands/NAME.md`; see [Extensions](#extensions). |
| `/quit` | Exit. Completed conversation state is already saved. |

Enter sends the input. Ctrl-J adds a line. Bracketed paste keeps multiline
text in the input area until Enter. Up/Down recalls input history.
Left/Right, Home/End, Backspace, and Delete edit the input. Ctrl-U clears it.

Ctrl-C stops an active request, approval prompt, or tool batch. At an idle
prompt, Ctrl-C exits. A cancelled shell command kills its process group.
Cancellation does not undo completed file changes. Check file state after
a failed write or command.

`NO_COLOR=1` disables styles, but keeps cursor control and input editing.
Use `chat -plain` for line input. Redirected output and `TERM=dumb` also use
plain mode. In plain mode, end a line with a backslash to enter another line.
The terminal state is restored on exit.

The default instructions tell the model to answer greetings and general
discussion without tools. Repository tasks still use tools as needed.
Settings, permission rules, hooks, MCP servers, custom tools, commands, and
skills are files; see [Extensions](#extensions). There is no plugin runtime.

## File context

Attach known files instead of spending model turns finding and reading them:

```sh
think run -file sum.go -file sum_test.go "Fix Sum and run go test ./..."
think run 'Review @sum.go @"notes with spaces.txt"'
think chat -file sum.go
```

`-file` can be repeated. In chat, these files apply to the first request
that passes file preparation. `@path` works in submitted prompts. Use quotes
for a path with spaces, or `@@` for a literal `@`. Email addresses, inline
code, and fenced code stay literal. Markers must be separate words; file
patterns are not expanded.

Text files must be regular UTF-8 text files without NUL bytes. The total text
content limit is 256 KiB. Missing, invalid, or oversized files stop the
request before a model call; content is not cut to fit. Repeated paths are
read once. Relative paths use the working directory, and absolute paths are
allowed. Contents are read again for each submitted request.

A `.png`, `.jpg`, `.jpeg`, `.gif`, or `.webp` file goes to the model as an
image, not as text. Its bytes must match its extension. Limits: 5 MiB for
each image and 8 images for each request.

The model receives the request and file snapshots in a marked data envelope.
The snapshots count as file reads, but the model can still request more
context. Chat stores the full envelope for resume. These files go to the
provider, just like tool results; attach only files you intend to send.

## Approval and access

File changes and Bash commands need approval by default:

- `y`: approve this call.
- `a`: approve later changes and commands in this chat.
- `n`, Enter, or Escape: deny this call. The model receives an error result.

The amber approval view shows the full command or file change, not raw tool
JSON. It states that tools have full filesystem access and are not sandboxed.
Writes state that an existing file can be replaced. Edits show old text in
red and new text in green. Color does not replace the `-` and `+` markers.
In plain mode, press Enter after `y`, `a`, or `n`; Escape is a terminal-composer
control.

`-yes` allows changes and commands without a prompt. Without a terminal,
mutating calls are denied unless `-yes` is set or a permission rule allows
them. `eval` requires `-yes` because both model tools and task checks can run
commands. Session-wide approval is not saved. `/clear` also resets it, unless
chat was started with `-yes`. Permission rules in the settings can allow or
deny calls without a question; see [Extensions](#extensions).

**These tools are not a sandbox.** They use your filesystem permissions.
Absolute paths are allowed. Read-only calls need no approval and can read
outside the working directory. Do not use this harness on untrusted tasks
that must be isolated from your files or network.

## Tools

| Tool | Input and behavior |
| --- | --- |
| `read_file` | `path`, optional 1-based `offset` and positive `limit`. Defaults: 1 and 2000. Returns numbered lines and an end or truncation notice. |
| `write_file` | Required `path` and `content`. Creates or replaces a file. Explicit empty content is valid; omitted or null content is rejected. |
| `edit_file` | Required `path`, `old_string`, and `new_string`. Changes exactly one match and reports the new line range. A missing match fails without a write and shows the closest current lines, numbered, so the model can retry without reading the file again; a duplicate match fails without a write. |
| `glob` | Required `pattern`; optional `path` and `limit`. Defaults: `.` and 1000. `**` matches zero or more path segments. |
| `grep` | Required Go regular expression `pattern`; optional `path`, `include` glob, `limit`, and `case_sensitive`. Defaults: `.`, 100 results, case-sensitive. Returns `path:line:text`. |
| `bash` | Required `command`; optional `timeout_seconds` or `background`. Default timeout: 120 seconds; maximum: 600 seconds. Reports output and exit status. Output above 32 KiB returns its first and last 16 KiB and the path of a file with the full output. With `background: true` the command starts as job N and the call returns at once. |
| `bash_job` | Required `action`: `list`, `output`, or `kill`; `id` for output and kill. Output returns what the job wrote since the last read, at most 32 KiB; the full output is in a file. Kill stops the whole process group. At most 8 jobs run; all jobs stop when the session ends. Needs no approval: it only reads or stops jobs that an approved `bash` call started. |
| `task` | Required `prompt`. Runs read-only research with the same model in a separate conversation. Returns findings to the parent. |
| `mcp_list`, `mcp_call` | Only when MCP servers are set. `mcp_list` starts a server on first use and lists its tools with their input schemas; `mcp_call` calls one. |
| `web_fetch` | Required `url`; optional `offset` and `max_chars`. Gets an http or https page and returns its text: HTML becomes plain text with headings, lists, code, and absolute links. At most 5 redirects, 30 seconds, and 5 MiB; long pages come in parts. Needs approval; the rule `web_fetch(*.go.dev)` matches the host. |
| `remember` | Required `fact`, one line of at most 300 characters. Adds the fact to `.inkling/memory.md`; the same fact again, apart from case and spacing, is not added. The next session loads the file into the system prompt; the current session does not, so the prompt cache stays valid. Needs no approval. |
| `ask_user` | Only in chat, where a user can answer. Required `question` and 2 to 6 `options`. Chat shows a numbered question; answer with a number or your own words. `run`, `rpc`, `acp`, and `eval` leave the tool out, so its definition costs no tokens there. |

The system prompt and the tool definitions go with every request, so they are
kept short. `/context` shows their current size.

Custom tools from `.inkling/tools/` follow the standard tools, in name order.

Read and search output is capped at 256 KiB. Bash output up to 32 KiB returns
whole. Larger output returns the first 16 KiB, a marker line, and the last
16 KiB. The marker gives the omitted byte count, the total size, and the path
of a `0600` file that holds the full output, up to 64 MiB. The model can read
that file with `read_file` and line offsets. Harness memory for one command
stays near 100 KiB, independent of its output size. Run and chat put these
files in `<session-id>.outputs/` in the session directory. One session keeps at
most 256 MiB of them: a new file removes the oldest files first, but never the
file that its own result names. A later read of a removed file fails with "not
found". At startup, chat and run remove the output folder of each session that
has no saved files and did not change for 7 days. A one-shot run never saves its
session, so its outputs stay for those 7 days. Eval puts outputs in a separate
temporary folder, outside the task working directory.

Glob and grep skip `.git`, `node_modules`, and symlinks. They do not implement
`.gitignore` rules. Grep skips NUL-containing files, files over 2 MiB, and
lines over 16 KiB; it reports skip counts. A basename-only grep `include`
pattern applies at every depth.

Read-only batches use at most four workers and keep results in call order.
Mixed or mutating batches run in order. A failed call stops the rest of that
batch and gives each unrun call an error result. Independent read-only calls
can still finish after another read fails. Tools start only after a complete
model response; partial streamed tool arguments are never executed.

Research tasks cannot write files, run commands, or start more research
tasks. They can use the read and search tools and take at most 20 model turns.
The parent waits for their results. `-tasks=false` removes the task tool.

When a background job ends, the next tool results or the next prompt tell the
model, once: the job, its exit code, and its last output lines.

## Extensions

Everything below is a file. With none of these files, nothing loads, nothing
starts, and the requests are the same as without the feature. Project files in
`.inkling/` take precedence over user files in `$XDG_CONFIG_HOME/inkling/` or
`~/.config/inkling/`. A bad file stops start-up with an error that names it.

**Settings**, `settings.json`. Unknown fields are an error:

```json
{
  "permissions": {
    "allow": ["bash(go test *)", "edit_file(src/**)", "mcp_call(github/*)"],
    "deny": ["bash(rm *)", "write_file(**/.env)"]
  },
  "hooks": {
    "before_tool": [{"command": "./scripts/check-tool.sh", "tools": ["bash"]}],
    "after_tool": [{"command": "./scripts/log-tool.sh"}],
    "prompt": [{"command": "git log -1 --format='Last commit: %s'"}]
  },
  "mcp_servers": {
    "github": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"],
               "env": {"GITHUB_TOKEN": "..."}, "description": "GitHub issues and pull requests"}
  }
}
```

- **Permission rules** are `tool` or `tool(pattern)`. For `bash` the pattern
  matches the command, with `*` for any text. An allow rule never matches a
  command with `;`, `&`, `|`, backticks, `$(`, `>`, `<`, or a new line unless
  the pattern has the same syntax at the same place, so `bash(go test *)`
  does not allow `go test ./... && rm -rf /`. A deny rule checks the whole
  command and each part between those operators. For `write_file` and
  `edit_file` the pattern is a path glob (`*`, `?`, `**`) relative to the
  folder; an allowed path must also stay in the folder after symbolic links
  resolve. For `mcp_call` it is `server/tool`. Deny wins over allow; without a
  match, chat asks. Rules are a guard, not a sandbox.
- **Hooks** run with `bash -c` in the folder and get the event as JSON on
  stdin. A `before_tool` hook that exits 2 refuses the call, and its stderr
  goes to the model; any other non-zero exit stops the turn, so a broken hook
  is never silent. The stdout of `after_tool` hooks goes after the result,
  so a formatter or linter can report problems to the model. The stdout of
  `prompt` hooks goes after your prompt as context. Each hook has 30 seconds.
- **MCP servers** speak the Model Context Protocol over stdio. The model sees
  only the server names and descriptions, plus two fixed tools, `mcp_list` and
  `mcp_call`. A server starts when the model first lists or calls it, and
  stops at exit. The tool list of a request never changes, so the prompt
  cache stays valid.

**Custom tools**, `tools/NAME.json`: `{"name", "description",
"input_schema", "command", "read_only", "timeout_seconds"}`. The command runs
with `bash -c` in the folder, gets the tool input as JSON on stdin, and its
output is the result; a non-zero exit is an error result. Tools that are not
read-only need approval like Bash.

**Commands**, `commands/NAME.md`: a prompt template that `/NAME args` sends,
with `$ARGUMENTS` replaced. Optional front matter: `description:` and
`read-only: true`, which runs the command like `/plan`. A command cannot use
the name of a chat command.

**Skills**, `skills/NAME/SKILL.md` with front matter `name:` and
`description:`. Only the name, the description, and the path go in the
system prompt; the model reads the file with `read_file` when a task needs
it, so a long skill costs nothing until it is used.

**Memory**, `memory.md`, project only: facts that the `remember` tool adds,
one per line. You can edit it. It loads into the system prompt at the start
of each session; above 4 KiB (about 1,000 tokens), the newest facts load.

## Undo and rewind

Before each prompt, chat records the conversation and, inside a git
repository, a snapshot of the working tree. `/undo` restores both from before
the last prompt; `/rewind` lists the prompts and `/rewind N` restores the
state from before prompt N. The snapshot is a git tree object written with a
temporary index: your index, `HEAD`, branches, and stash do not change, and
ignored files are not saved or touched. It runs while the first request
streams; only the first tool that can change a file waits for it.

A restore puts back the whole working tree, including your own edits made
after that prompt. Outside git, only the conversation goes back. Checkpoints
live in the chat process and are not saved with the session. Snapshot
objects are unreachable, so `git gc` can remove them after its grace period.

## Scripting and editors

`think run -json` writes one JSON object for each event on stdout:
`text`, `thinking`, `tool_call`, `tool_result`, `turn`, `status`, and last
`done` or `error`. `think run -plan` investigates with read-only tools and
prints a plan. `think run -goal "go test ./..."` works like `/goal`; it exits
with 1 when the check does not pass. Without a terminal, a change needs `-yes`
or an allow rule.

`think rpc` serves one conversation over stdin and stdout, one JSON object
per line, for editors:

```text
→ {"id":1,"method":"prompt","params":{"text":"Fix the failing test"}}
← {"method":"event","params":{"type":"tool_call","name":"edit_file",...}}
← {"id":"approve-1","method":"approve","params":{"tool":"edit_file","input":{...}}}
→ {"id":"approve-1","result":{"allow":true}}
← {"id":1,"result":{"final_text":"...","turns":4,"usage":{...}}}
→ {"id":2,"method":"cancel"}      → {"id":3,"method":"shutdown"}
```

One prompt runs at a time; a second one gets an error. History stays in the
process for later prompts and is not saved. At end of input the running
prompt is cancelled.

`think acp` speaks the [Agent Client Protocol](https://agentclientprotocol.com)
version 1 over stdin and stdout, for Zed and other editors. Each
`session/new` opens its folder with the same tools, rules, hooks, and memory
as chat. Tool calls stream as `tool_call` updates; a change asks
with `session/request_permission` (allow once, allow always, reject). Images
and embedded resources in prompts are supported; `session/load` and client
MCP servers are not. In Zed:

```json
{"agent_servers": {"Inkling": {"command": "think", "args": ["acp"]}}}
```

## Sessions and context

Chat saves full messages, thinking signatures, tool results, model, effort,
and token use. Files have mode `0600`; the session directory has mode `0700`.
The location is `$XDG_STATE_HOME/inkling/sessions`, or
`~/.local/state/inkling/sessions`. `think sessions -all` lists sessions from
all directories.

The provider bills reasoning in history as input tokens. At each new prompt,
the reasoning of every reply before the last one leaves the history, also in
the saved session; the last reply keeps it, because some APIs need it for an
open tool call. This costs one cache miss for the history at each prompt.

Each session has an append-only log, `<id>.log`, and a small index,
`<id>.meta.json`. A log record has a 4-byte length, a 4-byte CRC-32C, and a
JSON payload. The payload keeps the first K messages, adds new messages, and
stores the current metadata. A checkpoint appends only the messages that
changed and then calls `fsync`. The log is rewritten as one record by atomic
replacement when it would exceed twice the live history plus 64 KiB, for
example after compaction. `sessions` and `-resume last` read only the
indexes; they load one log.

After a crash, load ignores an incomplete final record or a zero-filled tail,
and the next save removes it. A bad record that is followed by more data is
reported as corruption, not skipped. The log is the authority; an index can
be one save behind after a crash.

Earlier releases stored one `<id>.json` file. These sessions still load. The
first save writes the new log and index, syncs them, and then removes the old
file. Older builds cannot read the log. To open a session in an older build,
run `think sessions export ID`. It writes `<id>.json` in the old format next
to the log and prints its path. If the older build changes that file, this build
loads it, because it uses whichever copy has the later update time. The next save
by this build writes the log again and removes the export.

A Linux check ran the race tests and a crash loop in Docker on Linux 6.10 arm64.
300 random `kill -9` signals interrupted 28,313 saves. After each kill, the
reloaded session was exactly the last confirmed save or the next one. Process
kills cannot reproduce a power loss; a unit test covers the zero-filled tail.

The loop saves the user prompt before a request and saves each completed
response/tool batch. A cancelled batch records both completed and unrun
calls. Partial model streams are not saved. Resume does not rerun recorded
tool calls. A process crash during a tool can leave changes that were not
yet recorded; inspect files before continuing. Sessions are not encrypted.
Do not open the same session for concurrent writes.

Successful turns do not repeat the last checkpoint. Read-only chat commands
do not rewrite the session. `-resume last` reads the newest matching session
without saving it again unless model or effort changes. Explicit session-ID
resume still updates its place in the recent-session list.

History is kept in memory as the exact JSON of each completed message. Requests
and session records reuse these bytes, so a completed message is encoded once.

At startup, the harness reads `AGENTS.md` and `CLAUDE.md` from the Git root
through the working directory, in ancestor order. Outside Git, it reads
only the working directory. Each instruction file must be a regular file
of at most 64 KiB. It also captures the Git branch, short status, and five
recent commit subjects. Git output is bounded and marked if truncated. In a
project of at most 200 files, the snapshot also lists them (from `git
ls-files`, or a walk outside Git; hidden paths and `node_modules` are left
out), so the model does not spend a turn on `ls`. This is a startup
snapshot, not a watcher. Guidance in other subdirectories is not loaded
automatically.

The built-in system prompt gives coding and approval rules. `-system FILE`
replaces that prompt; project context is still added. Use the same system,
`-thinking`, and `-extra` flags when resuming if you need the same settings;
those flags are not stored in the session. Model and effort are stored.
Changing models can invalidate stored thinking signatures on some providers;
use a new session for a different provider.

Automatic compaction starts at about 200,000 context tokens. It uses a byte
estimate and the last reported input count, not a local model tokenizer.
`-compact-tokens N` changes the threshold; `0` disables automatic compaction.
The summary replaces only older context. The newest message, or newest tool
call/result pair, stays exact. A failed or nonreducing summary leaves the
original history unchanged and reports an error. Summaries can lose detail;
verify source files again before making changes.

In chat, compaction runs in the background when a finished turn passes the
threshold, while you read and type. A resumed session that already passes the
threshold starts its compaction at the first key that you type, so reading the
last reply and leaving spends no tokens. Plain mode cannot see single keys; its
first turn applies the normal trigger. The terminal status shows background
compaction, and the footer shows the new estimate when it finishes. A later
prompt or command that uses history waits for it. Ctrl-C, `/clear`, and `/quit`
stop it. The result is applied only if the history did not change. A failed
background compaction is reported; the next request then uses the normal
trigger.

A summary request sends the same tool definitions as a normal turn and no
`tool_choice`, so the provider can reuse the cached prompt prefix. The prompt
tells the model not to use tools, and a summary with a tool call fails.

Known token use includes completed summary and research requests. Interrupted
streams can incur provider charges that these counters do not capture.
The UI reports tokens, not a dollar cost estimate.

## Model settings and retries

The default is `thinkingmachines/inkling-small`, without the `:free` suffix.
Use `-model` or `/model` to choose another model. Existing sessions keep their
saved model unless you override it with `chat -resume last -model MODEL`.

OpenRouter's DeepInfra route currently lists these prices in USD per million
tokens:

| Token type | Price |
| --- | ---: |
| Input | $0.45 |
| Output | $1.20 |
| Cached input | $0.10 |

Check the [live provider pricing](https://openrouter.ai/api/v1/models/thinkingmachines/inkling-small/endpoints)
before a large run. Repeated context and reasoning can increase token use.
These are token prices, not a guarantee of the total cost of a coding task.

Inkling Small has [open weights](https://huggingface.co/thinkingmachines/Inkling-Small)
under Apache 2.0, but it has 276B total parameters and 12B active per token.
The [vLLM serving recipe](https://recipes.vllm.ai/thinkingmachines/Inkling-Small)
lists at least 180 GB of total GPU memory for NVFP4 and 600 GB for BF16.
The development Mac has an M3 and 16 GiB of unified memory. It cannot hold
these weights on its GPU. No local inference path has been validated.

- `-effort low|medium|high|xhigh|max` sets `output_config.effort`.
- `-thinking JSON` sets the provider's raw thinking field.
- `-extra JSON` adds top-level fields. Managed field collisions are rejected.
- `-max-tokens` defaults to 16,384, including reasoning tokens. A reply cut
  at this limit cannot run its tool calls; it is dropped and the model is
  asked for a smaller step, at most twice for one prompt.
- `-max-turns` defaults to 50 for one user request.

The client makes at most five attempts for HTTP 408, 429, 500, 502, 503, 504,
and 529, before stream consumption. Backoff starts at 500 ms, doubles, adds
small positive jitter, and stays below 30 seconds. Valid `Retry-After` seconds
or dates take precedence. If the requested wait exceeds 30 seconds, the
original HTTP error is returned; the client does not retry early.

Cancellation stops a retry wait. Transport errors, HTTP 403, and failures
after successful response headers are not retried. This prevents automatic
replay after a partial streamed reply.

`-record DIR` records request bodies and raw response streams, without the
authorization header. Use a fresh, protected directory for each run: filenames
start at `0001` again, and records contain full prompts and tool output.

At startup, `chat` and `run` open the API connection with one `HEAD` request
to the messages endpoint. It sends no API key and no conversation data. The
first model request then reuses the connection. A failure of this request is
ignored; the first model request reports any real network failure.

## Timing and profiles

```sh
think run -yes -file sum.go -file sum_test.go \
  -timings timings.jsonl "Fix Sum and run go test ./..."
think timings timings.jsonl

think chat -cpu-profile cpu.pprof -runtime-trace runtime.trace
go tool pprof -top cpu.pprof
go tool trace runtime.trace
```

These options also work with `eval`. Timing is off by default. `-timings`
appends one JSONL record per submitted request, manual compaction, or
background compaction. Records
include model, requested effort, completion state, and nested stage times:
file preparation, JSON encoding, HTTP headers, DNS, connect, TLS, request
write, first byte, first useful model event, stream reads, stream decoding,
display callbacks, retries, tools, approval, compaction, and checkpoints.
First-useful time includes reasoning and tool events, not just visible text.
Request IDs separate parent and child model calls. Stream read, decode, and
display measurements are per-request totals, not per-token samples.

The report shows p50, p95, p99, sample counts, failures, cancellation, cache
use, and connection reuse. Missing provider token fields stay distinct from
reported zero. Failed and cancelled samples stay separate from successful
samples. Small sample sets do not establish tail latency. Stage times overlap;
do not add them to estimate wall time. Run timing excludes process startup,
idle input time, and writing the timing record itself.

Timing records contain no prompts, tool arguments, file paths, keys, or raw
errors. They use private `0600` regular files and reject symbolic links.
Each record is capped at 2,048 entries; overflow is reported. CPU profiles and
runtime traces use new `0600` files and refuse to replace existing files.
They can contain runtime paths and other process metadata. Profiling adds
cost; keep it off for ordinary use. Recording errors are fatal.

The client keeps the existing pooled HTTP transport. Real Small requests
used HTTP/2 and reused connections; no extra connection pool was needed.
The provider does not expose its physical inference device.

Tool schemas and required-field checks remain strict locally. The
[OpenRouter Messages schema](https://openrouter.ai/docs/api/api-reference/anthropic-messages/create-a-message)
accepts a tool `strict` field, but acceptance does not prove enforcement.
A real Small/DeepInfra probe with `strict: true` and
`provider.require_parameters: true` returned HTTP 200 and a tool call that
violated a required-field constraint. This harness does not enable strict mode
without a reliable provider guarantee. It does not guess missing arguments.
Tool examples make required fields explicit, and a failed edit prevents a
later check in the same batch.

## Measured performance

### History, sessions, compaction, and output

This update keeps history as encoded message bytes and appends only changed
messages to session logs. It also opens the API connection early, compacts in
the background while chat is idle, keeps compaction on the cached prompt prefix,
and saves large Bash output to a file. Each change was compared with the
previous binary on the same M3 host with Go 1.24.5. Hosted requests used Small
with high effort and the `deepinfra/fp8` route; fallbacks were disabled.
Response model and provider were checked. The provider does not expose the
physical GPU or precision.

Each chat resumed a generated saved history with 8, 128, or 1,024 messages,
then fixed the Go `Sum` bug after a 10-second pause before the first prompt.
Runs used fresh directories and alternated order. All 41 completed live chats
passed an independent `go test -count=1 ./...`. Test files stayed unchanged,
and answers kept history values from the first, middle, and last messages.

| Saved messages | First turn, before → after (median) | Peak harness RSS, before → after (median) |
| --- | ---: | ---: |
| 8 | 8.31 → 5.88 s | 18.14 → 18.33 MiB |
| 128 | 7.60 → 7.59 s | 20.03 → 19.31 MiB |
| 1,024 | 13.74 → 6.20 s | 23.10 → 20.52 MiB |

The 1,024-message gain comes from background compaction during the pause;
the old binary compacted after the prompt. These plain-mode runs started that
compaction at resume. The current build starts it at the first typed key in
the terminal interface, and plain mode compacts on its first turn, so the gain
now depends on typing time. The 8-message time difference
came from varying model turns, not from the harness. Every new-binary first
request reused the early connection. The old binary spent about 48 ms on DNS,
TCP, and TLS before its first request.

Heap profiles of the same tasks showed lower allocation and retained memory:

| Saved messages | Bytes allocated, before → after | Retained after task, before → after |
| --- | ---: | ---: |
| 128 | 9.23 → 4.64 MB | 1.37 → 0.85 MB |
| 1,024 | 14.03 → 7.36 MB | 1.43 → 0.51 MB |

For the 128-message task, request building fell from 2.89 MB to 0.41 MB
and session saves fell from 1.72 MB to 0.10 MB. The 1,024-message task
includes the one-time read of the old JSON file; that read is now its largest
cost. Five resumes of an already migrated 1,024-message log allocated
2.88 MB, against 3.49 MB for the old JSON file. Retained history was about
11% larger, because exact message JSON stays in memory. The early connection
adds about 100 KB at startup, which the old binary allocated at its first
request instead.

A summary request after a normal turn used about 147 new input tokens and
20,800 cached tokens. The old summary request used about 19,500–20,000 new
input tokens and at most 384 cached tokens. At this size, the summary still
took about 4 seconds. An earlier attempt sent `tool_choice: none`; the
provider then dropped the cached prefix, so that design was not kept. When a
finished turn passed a lowered threshold, the old binary spent 3.40 s on
compaction at the start of the next turn. The new binary ran it in 2.05 s
while idle, and the next turn did not wait.

A session printed 8 MiB three times and cancelled three running shell
commands. Each large result used 33 KB of history instead of 66 KB. The saved
files matched the command output byte for byte and had mode `0600`. The model
read one with `read_file`. Each recovery-turn request used 47,760 input tokens
instead of 71,365, and peak harness RSS fell from 23.09 to 21.58 MiB. Each
cancellation stopped the shell and child processes in about 20 ms.

A real TUI run showed the background status and then the reduced footer
estimate, completed the task, and passed the independent test. `eval` passed
2/2 tasks and removed its temporary directories. These samples do not
establish stable p95 or p99 latency. The raw records stay private.

### File context and local costs

Five paired runs per fixture compared the prior binary with the file-context
update on the same M3 host. Both versions used Small through OpenRouter,
requested high effort, and recorded streams. The new version also attached
the named files and recorded stage times. Run order alternated; each run used
a fresh directory and an independent check. All 20 runs passed, and protected
test files stayed unchanged.

| Task | Before, median | After, median | Model turns, median |
| --- | ---: | ---: | ---: |
| Python clamp, nine boundary checks | 6.03 s | 4.30 s | 6 → 4 |
| Go Sum, four test cases | 4.64 s | 3.85 s | 5 → 4 |

Total model requests fell from 56 to 41. Missing-required-field calls fell
from one to zero in this sample; this does not establish an error rate.
The model still sometimes rereads an attachment or puts edit and check calls
in separate responses. Same-batch failure handling does not stop a new call
in a later model turn; approval still applies to each mutating call.

All 41 measured requests used HTTP/2. The 31 requests after each process's
first request reused a connection. Forty requests reported cache reads.
A separate CPU profile had 50 ms of samples over 3.67 seconds; its runtime
trace showed network reads as the main wait. These are hosted model runs.
The provider reported DeepInfra, but did not expose its physical GPU.

A host-only session check used 24 saved sessions, each with 512 messages and
about 1.1 MB of JSON. After one warm-up pair, five pairs measured resume,
ten read-only commands, and exit. Median time fell from 499 ms to 351 ms.
The new version did not change any session bytes or timestamps. Complete
history and token use stayed intact. This is not an inference benchmark.

A real TUI run also read a 50,000-character Bash result through a PTY limited
to 1 KiB per 20 ms. Display callbacks took 1.23–1.30 seconds in both ordinary
and verbose mode. Bash output is visible in both modes. This output-device
wait remains; no asynchronous display queue hides it. A normal native SSH
reply used about 1.2 ms in streamed display callbacks. No journal or new
transport layer was added.

These small coding samples do not establish stable p95 or p99 latency.
The raw records stay private.

### File-edit copies

`edit_file` now matches and replaces bytes without converting the whole
file to a string and back. The exact-match rule, errors, file permissions,
and tool output stay the same.

Real-file measurements on the M3 with Go 1.24.5 showed these allocations:

| File size | Before, allocated per edit | After, allocated per edit |
| --- | ---: | ---: |
| 1 MiB | 4.01 MiB | 2.01 MiB |
| 8 MiB | 32.01 MiB | 16.01 MiB |

Each sample had two untimed warm-up edits and checked all file bytes and
permissions afterward. Short and longer runs had variable elapsed times.
These allocation results do not establish a complete-task speedup.
Four real hosted-model tasks also passed independent Go tests. The 8 MiB
tasks kept the large comment unchanged.

Two proposed prompt changes were not kept. Fewer model requests did not
produce a reliable gain in time to an independently checked result.
The prompt and attachment format remain unchanged. The raw records stay
private.

### Memory across sessions

Memory profiles used generated saved histories with 8, 128, and 1,024
messages, followed by a real Go bug-fix task. The M3 host used Go 1.24.5.
Hosted requests used Small, high effort, and the `deepinfra/fp8` route with
fallbacks disabled. Response model and provider were checked.

JSON encoding was the largest allocation source in the long-session
profile. Message encoding now writes each content block into the message
buffer instead of keeping an intermediate list of encoded blocks.
Four recorded requests produced identical wire bytes before and after.
Three paired measurements per request, each with 40 measured encodes
after a warm-up, gave these median results:

| Recorded request | Allocated bytes, before → after | Encoding time, before → after |
| --- | ---: | ---: |
| Short history | 29,209 → 23,217 | 60 → 41 µs |
| Medium history | 348,056 → 246,743 | 707 → 411 µs |
| Long-history compaction | 2,886,160 → 2,248,191 | 5.48 → 3.16 ms |
| After large output and cancellation | 1,201,170 → 850,878 | 2.29 → 1.29 ms |

These are host-side encoding measurements, not model throughput. Separate
normal-build runs had no heap profiler or forced garbage collection.
External RSS samples were taken about every 100 ms and at task boundaries.
Two paired runs per history size gave these median observed parent peaks:

| Saved messages | Before | After |
| --- | ---: | ---: |
| 8 | 18.20 MiB | 18.30 MiB |
| 128 | 20.03 MiB | 20.63 MiB |
| 1,024 | 23.40 MiB | 22.69 MiB |

There was no consistent resident-memory reduction across these cases.
Child-process RSS is recorded separately; it is not included in this table.
Sampling can miss short peaks. These runs do not establish tail latency.

The default automatic compaction remained enabled. Long sessions exercised
it; values from the first, middle, and last saved messages remained available.
Repeated-output checks copied and printed 8 MiB three times per session.
Each saved result kept the existing 64 KiB limit and the exact omitted-byte
count. Twelve cancellation checks stopped both shell and child processes.
The profiled sessions returned to seven goroutines after each cancellation.
Independent Go tests passed after recovery.

All 22 completed plain-chat runs and one real TUI run passed independent
Go tests. Test files stayed unchanged. The TUI returned to its ready prompt
and exited normally. No model setting, context limit, GC setting, dependency,
or storage format changed. Temporary profiling code was removed. The raw
profiles and records stay private.

### Earlier UI comparison

A local comparison used an Apple M3 with 16 GiB of memory, macOS 26.6.1,
and a 100-column by 32-row PTY. Each client had one warm-up, then five
startup runs in rotating order. Startup ends when the first typed character
appears in raw input mode. It excludes first-run setup and makes no model
request. Memory is peak process RSS, not the sum of every child process.
Input timing ends at PTY output, not at the terminal emulator's screen update.

| Client | Warm startup, median | Input echo, median | Peak RSS, median |
| --- | ---: | ---: | ---: |
| Inkling, UI update | 35 ms | 0.016 ms | 12.2 MiB |
| pi 0.87.1 | 196 ms | 0.399 ms | 132.4 MiB |
| oh-my-pi 18.6.1 | 531 ms | 34.479 ms | 557.6 MiB |
| Claude Code 2.1.205 | 330 ms | 15.966 ms | 358.2 MiB |

Five paired 120-line display replays reduced median render time from
471 ms to 15.6 ms. Terminal output fell from 2.21 MB to 0.25 MB. This is a
burst-render measurement of the real terminal code, not model throughput.

A separate real coding comparison fixed an interval-clamp bug and checked
nine boundary and negative-limit cases. All four clients passed all three
measured runs. Median task times were Inkling **4.17 s**, pi **4.23 s**,
oh-my-pi **7.81 s**, and Claude Code **9.35 s**. Each used paid
`thinkingmachines/inkling-small` through OpenRouter's Messages API and
requested high effort. Client system prompts and request details differed.
The provider's physical device was not exposed.

Inkling and pi were effectively tied on this small task. These results do
not prove a general coding-speed or quality lead. They do show lower local
UI overhead in this setup. The raw records stay private.

## Verification

```sh
go vet ./...
go test -race ./...

# Real model evaluation. Uses temporary working directories, not a sandbox.
think eval -yes -out out/results.jsonl evals/tasks.jsonl

# A task can also have a "files" list. Paths use its copied working directory.
# Prompt @path markers use the same attachment rules as run and chat.
```

The Go tests cover host logic, protocol handling, tools, session storage, and
project context. A separate terminal smoke run checked approval, denial,
Ctrl-C cancellation, shell process cleanup, parallel search, read-only child
tasks, automatic/manual compaction, and resume against a local protocol
fixture. These checks do not prove real model behavior.

A real PTY run used `./inkling chat -model thinkingmachines/inkling-small -max-turns 6`.
It checked a greeting without tools, exact file contents after writes and
edits, Bash approval and denial, and cancellation. It also checked multiline
paste, history, resize, `NO_COLOR`, resume, `/tools`, and exact terminal-state
restoration. These requests used OpenRouter, not a local protocol fixture.

The display update also passed a real Small chat with `-max-turns 8`. The
run checked complete write/edit approvals, exact file contents, Markdown
output, Escape denial, cancellation of an approved shell command, discarded
input while busy, Unicode cursor positions, history, resize, `/tools`, and
exact terminal-state restoration. Separate runs checked `NO_COLOR` and plain
mode. The updated Linux arm64 binary was cross-compiled; its terminal path
was not run on Linux.

The same binary ran in native Terminal over SSH on the Mac mini M4 with
macOS 26.5.1. `chat -resume last -model thinkingmachines/inkling-small` restored
the saved chat. A real reply rendered a heading and two bullets. The empty
input box stayed hidden while the model worked, then returned for input.

The real evaluation with `thinkingmachines/inkling-small` passed 2/2 tasks
with zero errors and nine model turns. It created the required file and
passed the Go tests for the bug-fix fixture. The run took 241.72 seconds,
including one slow model request; this is not a throughput benchmark.
Command: `think eval -model thinkingmachines/inkling-small -yes -out results.jsonl evals/tasks.jsonl`.
The earlier Inkling free evaluation failed with HTTP 403 before inference.

The file-context update also passed a real attached-file `eval`, including
the independent Go check and its timing record. A real PTY chat checked
quoted file paths, literal inline `@` text, exact edited bytes, approval
denial, active-command cancellation, missing-file rejection before HTTP,
manual compaction, exact resume, and terminal-state restoration.
A native M4 chat used the attached Sum source and correctly reported its
skipped first element. The same-batch failure regression uses real local
files; the provider used separate turns in the interactive sample.

The extension release was checked against the real model
(`thinkingmachines/inkling-small`, high effort, one provider):

- One `run -json` task without `-yes` used every extension type: an allow
  rule ran `go test` and an MCP call without a question, a deny rule refused
  `rm -rf build`, a custom tool counted words, the real
  `@modelcontextprotocol/server-everything` server started on first use and
  answered `echo`, the model read a skill file, and a prompt hook added
  context. Before-tool and after-tool hooks ran for each tool that ran. The
  MCP server process ended with the run.
- `rpc` answered two prompts with one approval round trip and kept history.
- `run -plan` replied with a plan; both change attempts were refused and the
  file stayed byte-identical.
- An attached PNG reached the model, which named both of its colors.
- A background `python3 -m http.server` job served a request, was killed by
  `bash_job`, and nothing listened on its port after the run.
- In the real chat TUI, `/undo` restored the edited file byte for byte, a
  read-only user command refused a change, and `/rewind` listed the prompts.

The 20-task eval, one run each, with the same settings:

| Build | Passed | Turns | Turn latency p95 | Uncached input | Output tokens |
| --- | --- | --- | --- | --- | --- |
| v0.3.0 | 19/20 | 159 | 5.06 s | 60,380 | 56,022 |
| extensions | 20/20 | 173 | 3.71 s | 52,260 | 23,101 |

The v0.3.0 failure was one runaway reply that hit `max_tokens`. One run of
each build cannot separate a gain from model variance; it shows no
regression from the longer tool list. `scripts/limits` on the release build:
7.15 MiB, 25.7 ms startup (median of 7), no measurable idle CPU.

v0.5.0 was checked with the same model settings, with real `gopls`, real
processes, and a real terminal. (v0.6.0 removed its language-server
diagnostics and model roles.)

- Writing a Go file with a type error gave the model
  `bad.go:3:25: error: cannot use "x" ...` from `gopls`, then the output of
  an `after_tool` hook, in the same tool result.
- `run -goal "go test ./..."` on a task that left a failing test: the check
  failed, the model got its output, fixed the code, and the check passed.
- `web_fetch` returned the text of a real page. `remember` wrote a fact; a
  new session answered with it and used no tools. A background job that
  exited with code 3 reached the model as a notice in the next request.
  Without a user, `ask_user` told the model to choose, and it stated its
  choice.
- In the chat TUI in a PTY: `/title`, `/context`, a numbered `ask_user`
  question answered with `2`, `/goal` with one failed round and then a pass,
  `/fork`, and `-resume pick`; `think sessions` showed both titles.
- A scripted ACP client: `initialize`, `session/new`, `session/prompt`, two
  `session/request_permission` round trips, tool updates, `end_turn`, and the
  file on disk.
- `-task-model` and `-compact-model` sent the research and summary requests
  to `thinkingmachines/inkling`, and the other requests to the main model.

The v0.5.0 20-task eval, 3 runs of each build, all builds at the same time in each
round. Each value is the mean of one run of 20 tasks:

| Build | Passed (of 60) | Turns | Input tokens | Output tokens | Turn p95 |
| --- | --- | --- | --- | --- | --- |
| v0.4.0 | 59 | 157 | 550k | 31.4k | 3.40 s |
| v0.5.0 | 58 | 157 | 583k | 32.7k | 3.51 s |
| v0.5.0, `-diagnostics=false` | 58 | 158 | 589k | 37.6k | 4.06 s |
| v0.5.0, hashline edits | 59 | 191 | 815k | 28.7k | 2.99 s |

Every failure in every build was a reply that hit `max_tokens` in a tool
call, not a wrong result. Against the same release with replace edits, the
hashline edit format (line anchors instead of old text) took 22% more turns
and 40% more input tokens, so it was removed. Diagnostics fired in 4 of 20
tasks of a recorded run, each on a real compile error; with them, output
tokens were 13% lower and turn p95 was lower, which is inside the
run-to-run spread but worse on no measure. The three new tools add about 6%
input tokens.

v0.6.0 cuts tokens. The system prompt went from 386 to 202 tokens and the
tool definitions of `run` and `eval` from 1,883 to 967 tokens, measured with
real requests: shorter texts, no `list_dir` (`glob` and `ls` cover it), and
no `ask_user` where no user can answer. Reasoning of earlier replies leaves
the history at the next prompt; a recorded two-prompt `rpc` run showed the
second prompt's requests without it, and the model still answered from the
history. The 20-task eval, 3 runs of each build at the same time:

| Build | Passed (of 60) | Turns | Input tokens | Output tokens | Wall time |
| --- | --- | --- | --- | --- | --- |
| v0.5.0 | 57 | 156 | 570k | 36.6k | 313 s |
| v0.6.0 | 59 | 146 | 360k | 21.3k | 236 s |

Input tokens fell 37%, output tokens 42%, and wall time 25%; each round
showed both drops. The v0.5.0 failures were replies cut at `max_tokens`; the
v0.6.0 failure was one wrong result. The `think` process stays near 20 MiB;
a resumed 198k-token session peaks at 18 MiB. `scripts/limits`: 7.42 MiB,
27.7 ms startup, no measurable idle CPU.

v0.7.0 removes wasted turns found in a recorded eval round of v0.6.0, where
17 of 20 tasks spent their first turn on `ls` or `glob`, 129 of 133 tool
turns made one call, and one task failed 8 edits on text that an earlier edit
had changed:

- The project snapshot lists the files of a small project.
- The prompt asks for all reads in one response and an edit with its test,
  and for short reasoning.
- `edit_file` reports the new line range, and on a miss shows the closest
  current lines.
- A reply cut at `max_tokens` is retried with a note instead of failing the
  task. In a real run with `-max-tokens 80` the cut edit came back smaller
  and the test passed.
- `todo_write`, which no task called, left the tool list.

| Build | Passed (of 60) | Turns | Input tokens | Output tokens | Wall time |
| --- | --- | --- | --- | --- | --- |
| v0.6.0 | 58 | 160 | 427k | 29.1k | 262 s |
| v0.7.0 | 60 | 136 | 314k | 22.0k | 215 s |

Turns fell 15%, input tokens 26% (lower in every round), output tokens 24%
(lower in 2 of 3 rounds), and wall time 18%. The v0.6.0 failures were a
reply cut at `max_tokens` and a task that ran out of turns. `scripts/limits`:
7.42 MiB, 23.9 ms startup, no measurable idle CPU.

## Layout

- `cmd/think`: the `think` command: login, interactive chat, sessions, one-shot runs, JSON events, RPC, ACP, goals, questions, evaluation, and extension wiring.
- `internal/anthropic`: streaming Messages client, encoded history messages, images, and bounded retries.
- `internal/agent`: conversation loop, tool gate, approval hook, compaction, and research tasks.
- `internal/terminal`: terminal input, live display, resize, and state restoration.
- `internal/presentation`: safe text, Markdown styles, tool views, and approval details.
- `internal/tools`: local coding tools and background jobs.
- `internal/extend`: settings, hooks, custom tools, commands, skills, project memory, and goal checks from files.
- `internal/permission`: permission rules for tool calls.
- `internal/mcp`: MCP client over stdio.
- `internal/web`: the `web_fetch` tool and its HTML-to-text conversion.
- `internal/rewind`: working-tree snapshots for undo.
- `internal/session`: private append-only conversation storage.
- `internal/project`: project instruction and Git status.
- `internal/attachment`: explicit file and image context preparation.
- `internal/latency`: opt-in stage records, HTTP trace hooks, and reports.
- `internal/openrouter`, `internal/credentials`: key check and local key storage.
- `internal/record`, `internal/eval`: recording and task evaluation.
- `prompts`, `evals`: optional prompts and evaluation fixtures (20 tasks).
- `scripts/release.sh`, `scripts/limits`: release builds and the startup, idle-CPU, and binary-size limits that a release must pass.
