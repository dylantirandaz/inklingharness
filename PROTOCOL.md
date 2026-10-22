# Protocol: harnesses on Inkling Small

Version 1, October 20, 2026. Change this file only by a dated entry in the change log at the end.

## 1. What the benchmark claims

The author of Think wrote this protocol. Think is built for Inkling Small. The other harnesses are not. So the benchmark makes claims about harness and model together, and it says so.

| Scoreboard | Question | Rule |
|---|---|---|
| A. Out of the box | What does a user get today from each harness on Inkling? | Each harness runs as shipped, set up as its vendor documents for another model, at one reasoning effort. |
| B. Tuned | What can each harness do on Inkling with care? | Each harness gets the same bounded tuning budget on the dev split, then is frozen. |
| C. Home models | Is the gap on Inkling a gap in design or a gap in fit? | Each generic harness also runs on the model it was built for, against the baseline on that model. |

Round 1 is scoreboard A. Scoreboards B and C are planned. A claim must name its scoreboard. The claim of the author is co-design: on Inkling, a harness built for it is better or cheaper than a harness that is not. It is not a claim that Think is a better harness in general.

## 2. Splits and the seal

`evals/splits.json` puts every task in one split. `python -m evals.splits --check` fails when two splits overlap or when an agent that calls a model has run on a final-test task.

- dev: the 91 tasks of the airgapped runs (38 SWE-bench and 53 Terminal-Bench). Think was designed while these runs were read, and Think variants for compaction and output limits ran on the Terminal-Bench tasks.
- round1: the 232 SWE-bench tasks of round 1. They were held out for the frozen Think build `af9108f360ff`. The task order was fixed on October 19, after the build was frozen on October 18, and before any agent ran on them. Any Think change that uses them for tuning makes them dev for that change. Round 1 results stay valid for the frozen build. 22 of them are stress tasks.
- final_test: SWE-bench tasks that no agent has run on. The two checks that choose them (the reference solution and the sandbox probe) call no model. There are 216 of them (the valid tasks of positions 241 to 460 of the fixed order; 4 of the 220 are excluded, 3 for a copy of the package outside the test folder and 1 because the reference solution fails), and 21 are stress tasks (difficulty "1-4 hours" or longer in `task.toml`). They are used once, for the final comparison. With 216 tasks, 80% power needs a gap of about 7 points when 12% of the tasks are discordant, and about 9 points when 18% are. So the final test finds a large gap only. A smaller gap needs more tasks, from a newer set (section 3).

Rules:

1. Tune a harness only on dev and round1.
2. Freeze the build (SHA-256) before the final test. Do not change it after the first final-test trial starts.
3. Run the seal check before and after the final test.
4. A harness that fails on the final test is not fixed and run again. A fault of the environment is run again (section 6).

## 3. Tasks

- The tasks of SWE-bench Verified are short: the median reference patch has 6 changed lines, and 12 of 232 tasks touch 3 files or more. A one-tool loop does almost as well as a full harness on them (round 1: baseline 70.7%, Think 73.7%). So they cannot show most of what a harness adds.
- The stress subset is the tasks that the metadata calls "1-4 hours" or longer. It is chosen from `task.toml`, never from a result. It is reported on its own.
- A benchmark that shows harness value needs long runs, large repositories, context pressure, and recovery from errors. The Harbor registry has such sets: `scale-ai/swe-bench-pro`, `swe-rebench/swe-rebench-leaderboard`, `ibragim-badertdinov/swe-rebench-07-2026` (July 2026, probably newer than the model data), `snorkel-ai/senior-swe-bench-v2026.06`, and `abundant/swe-marathon`. Before a set is used, its tasks must be closed (`evals.closed_tasks`) and must pass the two checks without a model (`evals.validate_tasks`).
- SWE-bench Verified has been public for years. A model can have seen its tasks. The same bias applies to every harness, but a newer set is a better test.

## 4. Harness setup

- Native: every harness uses its own tools, prompt, and settings. A failure that comes from how a harness and the model fit each other counts as a result. Examples: Claude Code writes a tool call as text when the model fills its `pages` field; Codex sends an image that the provider rejects with a 422.
- The setup follows the vendor documentation for a third-party model. A difference that does not change the request (for example `x-api-key` against `Authorization: Bearer`) is checked by capturing the request in both forms.
- One reasoning effort for all harnesses (`high`) for scoreboard A. Effort is the model's compute dial, so a comparison at one effort shows one point of each harness. On Inkling Small, Think at `low`, `medium`, and `high` solved 38.4%, 56.5%, and 73.7% of 232 tasks and cost $0.0038, $0.0083, and $0.0684 a trial, and at `max` a trial cost at least 4 times the cost at `high`. So the cost and the success of a harness must also be reported at more than one effort (scoreboard D: the frontier over `low`, `medium`, and `high`), for the harnesses whose request sets the effort (Think, Claude Code, Codex, and the baseline; Pi sends a thinking budget that the model ignores). Versions and builds are pinned and recorded with their SHA-256 in `out/terminal-bench/native-run-identity.json`.
- The instruction is the task text, with no wrapper, for every harness. The baseline (`evals/bare`) has one tool and no system prompt. It shows what a harness adds to a bare loop.
- Before a final run, send each harness maintainer the setup and the pilot failures. Accept a change once, if it follows their documentation. Record it, and apply it to the pilot again.

## 5. Run rules

- The agent phase is closed: the only open host is `openrouter.ai`. The audit (`evals.audit_trials`) marks a trial invalid when the agent calls the model API itself, uses a web tool, or retrieves the answer. An invalid trial counts as not passed.
- The API key is confined to the model under test (`python out/terminal-bench/with_key.py evals.preflight`, and `--require-confined-key` on `evals.harbor_run`). Round 1 did not do this, and the audit was the only control. Steps: create a guardrail in the OpenRouter workspace with the model allowlist, create a new key under it, and save the key in `~/.config/inkling/benchmark-key`.
- All harnesses of a tranche run at the same time, so that the provider conditions are the same. A run starts only when `python -m evals.provider_health` passes, and it runs under `evals.budget_guard` with `--stop-on-degraded-provider`, `--min-cache-hit`, and the credit and key-limit floor.
- Tasks are run in tranches of 20 from a fixed order. One attempt per task. More tasks tell more than more attempts at the same tasks.

## 6. Faults, failures, and the pilot

- A trial that the environment or the provider ended is a fault, and it runs again (`evals/trial_faults.py`). Every other end counts. An exception that the rule does not know is a decision for a person.
- Before each phase, run a pilot of 25 trials for each harness. Read the failures by kind with `python -m evals.failure_taxonomy JOB...`. Look for kinds that come from fit and not from coding ability: `tool_call_as_text`, `provider_rejected`, `no_edit`, `fault`. Fix the setup if it is wrong. Do not fix the harness.
- The pilot trials are not part of any score.

## 6b. Screening a change to Think

These rules are written before the screen of the prompt variants below runs, so that the result cannot choose the rule.

- A change is screened on round1 (dev), never on final_test. It changes one thing at a time, or it is one named variant.
- Screen at `medium` effort first. A run costs about $0.0085, so 232 tasks cost about $2, and 6 points of success are visible (a gap of 14 net tasks, exact paired test p < 0.05). The control is the existing `medium` run of the same build and prompt (131 of 232).
- The variants of the first screen, fixed here: `evals/prompts/think-v1-workflow.txt` (the built-in prompt and a five-step workflow: read, reproduce, change the cause, run the reproduction and the nearest tests, read the report again) and `evals/prompts/think-v2-workflow-minimal.txt` (V1 and the rule to make the smallest change). The control is `evals/prompts/think-v0-builtin.txt`, the text that the frozen build holds.
- A variant is promising if it solves at least 14 more tasks than the control at no more than 1.5 times its cost. It is harmless if it is within 5 tasks of the control at no more cost. Anything else is rejected.
- A promising variant is run again at `high` on 120 tasks of round1 (about $8) against the existing `high` runs. It must not lose more than 2 tasks there, and its cost must not rise more than 15%.
- A variant that passes both goes to the final test once, with the build frozen. The final test of the frozen build at `high` is the claim. A change that was tuned on round1 is not a held-out result on round1.
- Rejected variants are listed in the README with their numbers.

Result of the first screen (October 20, `medium`, 232 tasks): control 131, V1 129, V2 134, and the baseline 123. Neither variant reached the 14 tasks that the rule asks for, and both cost 16% to 18% more, so both are rejected. The model at a given effort does not take a procedure from the prompt: the turns rose from 11.6 to 12.9. Nothing went to the `high` screen, and the Think build and prompt of record are unchanged.

Candidates that remain open, with the cost of one screen (a `medium` screen of 232 tasks is about $2; a `high` screen of 120 tasks is about $8):

- The lean profile (`think run -lean`), built on October 20 and not yet screened; see below.
- A check of the patch before the end (a second reading of the report against the diff, with the diff given to the model): the wrong fixes are 66 of 85 failures, and the model's own tests do not tell them apart, so this is the one candidate that works on the main failure. It costs extra turns at `high`, and it needs a `high` screen on 232 tasks (about $16).
- Scoreboard D, the frontier over efforts for the other harnesses: Claude Code, Codex, and the baseline at `low` and `medium` on 232 tasks, about $11.

Screen of the lean profile (written on October 20, before the run):

- The profile: `read_files` (up to 8 files or ranges in one call), `read_file` with 400 lines by default (2,000 in the standard set), bash output cut at 8 KiB (32 KiB in the standard set; the full output stays in a file), no `task`, `remember`, `inspect_images`, or `context_checkpoint` tool (`context_read` stays for automatic compaction), a prompt without the lines about parallel calls and the left-out tools, and a short note in place of a read that repeats an earlier read of the same run with the same input and the same result (the second repeat gets the full text again; compaction clears the memory). The first request is 1,253 characters shorter in its tools and 487 in its prompt. Reasons: Inkling Small makes one tool call in a message; 43.5% of the turns of Think were a read that followed a read; old tool output was 70% of the cached input, and results over 8,000 characters were 22% of it; repeated reads were 3.5% of the tool-result characters of round 1 (`TestReplayRepeatedReads`).
- Build: `out/terminal-bench/think-lean-linux-amd64`, SHA-256 `ecb5ee2ce1bbac5fb04e3f7696b7d4ceb6676d86f27d47fdf66c088a6dd2d226`, staged on the volume as `think-lean-linux-amd64-ecb5ee2ce1bb`. Without `-lean`, this build sends the same request bytes as the frozen build `af9108f360ff`: the first request, and the four requests of a scripted run with tool calls, were captured from a native build of this tree and of the tree that builds `af9108f360ff`, and they are equal. This build replaces `874a0dca36f8` (October 20), which never ran on the model.
- Run: config `evals/dev-think-lean-high-120.json`, `-lean` at `high`, on the 120 tasks of `evals/dev-pilot-tasks.json` (the 22 stress tasks of round1 and 98 other round-1 tasks chosen at random with seed 20261020).
- Control: the round-1 trials of the frozen build at `high` on the same tasks: 84 of 120 passed, $0.0732 a trial, median agent time 119 s.
- Speed is measured in turns, not seconds, because the speed of the provider changes during the day (68 to 176 tokens a second on October 20) and the control ran on October 19 and 20.
- Accept the profile for headless runs on Inkling if all of these are true: the cost per trial is at least 10% lower than the control, the mean number of turns is not higher, and the lean run passes at least 82 tasks (no more than 2 fewer than the control).
- If the cost is at least 10% lower and the lean run passes 79 to 81 tasks, the result is open: run the other 112 tasks of round1 the same way (about $7), and apply the rule to all 232 tasks, where the lean run must pass at least 167 (no more than 4 fewer than the 171 of the control). Below 79 passes, or with less than 10% saving, the profile is rejected.
- The lean profile is a tuned Think. It does not replace the frozen build in scoreboard A, and round1 is dev for it.

## 7. Statistics

- Primary: the success rate over tasks, and the exact paired test of Think against each other harness on the same tasks. These four comparisons are named in advance. Report Holm-adjusted p values next to the plain ones.
- Secondary: dollars, agent time, turns, and tokens per attempt, and the same on the tasks that two harnesses both passed (`evals.harness_tax`, `evals.compare_arms`). Report the Pareto frontier of cost and success.
- Sample size (`python -m evals.power_analysis`). With 12% of the tasks discordant between two harnesses, 80% power at p = 0.05 needs 375 tasks for a 5-point gap, 1,045 for 3 points, and 2,353 for 2 points. With 18% discordant, it needs 563, 1,568, and 3,530. Round 1 had 232 tasks, so it could find a gap of about 7 points. An earlier note in the README gave 700 to 1,200 tasks for 2 points. That figure is the width of the interval, not the power for a test.
- A gap is called clear when its p value is under 0.05 after the Holm adjustment, and its interval does not hold 0. Other gaps are reported as not separated.

## 8. Reporting

- The headline is the held-out result of the scoreboard that the claim names. Pooled results that include dev tasks are secondary and say so.
- Report cost, time, and the failure kinds next to success. Report negative results.
- State the conflict of interest in the first paragraph. List what was changed after a result was seen, and why.

## 9. Conflict of interest

The author wrote Think and is the user of this repository. Controls: the harness build is frozen and hashed before the tasks are chosen; the tasks are drawn from a fixed random order; all harnesses run through the same runner and the same fault rule; the faults and the audit changes are listed in the README with their reasons; the competitor failures were checked against a replay before they were explained (Claude Code, `pages`); and the data are published (section 10).

## 10. Artifacts

Publish: the configs and their SHA-256, the Think and baseline builds and the sources, the task lists and splits, the job folders (`result.json`, agent logs, verifier reports), the audits, the fault manifests, the results files, and `native-run-identity.json`. Ask a third party to rerun a random sample of 40 tasks of the final test for every harness, with their own key, and compare the results.

## 11. Open items for the owner

- Create the confined key (section 5).
- Send the setups and pilot failures to the maintainers of Claude Code, Codex, and Pi (section 4).
- Choose the home models and the budget for scoreboard C. Claude Code on a small Claude model and Codex on a small GPT model keep the cost near the cost of Inkling runs.
- Decide the tuning budget for scoreboard B. Without it, only Think is tuned, and the claim must say "tuned Think against out-of-the-box harnesses".
- Give the assistant its own OpenRouter account or key. On October 20 the credit of the account fell about $0.24 a minute with the benchmark key idle, so the credit floor of the guard counts other usage, and a budget for the benchmark is not known.

## Change log

- 2026-10-20: version 1.
- 2026-10-20: added the effort finding to section 4, section 6b (screening a change to Think) with its first result, the cost of the open candidates, and the shared-credit item of section 11.
- 2026-10-21: before any lean run, the lean screen of section 6b moved to build `ecb5ee2ce1bb`, which adds the repeated-read note to the profile, a faster start (git commands in parallel), and less work and memory for each turn; the request bytes without `-lean` stay equal. The decision rule did not change.
