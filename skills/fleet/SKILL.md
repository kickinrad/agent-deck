---
name: fleet
description: Fan out a fleet of independent agent-deck child sessions from inside a session and check their progress non-blockingly. Use when the user wants to "launch several/N sessions", "fan out", "run agents in parallel", "spin up a fleet", "kick off background agents", or "check progress from the main session" without blocking — covers launching parented children, polling status + completion via `session children`, and collecting results via `session output`.
metadata:
  compatibility: "claude, codex, opencode"
---

# Fleet

Fan out several independent agent-deck child sessions from inside your current
session, keep working, and check their progress on demand — **without blocking**
and **without consuming any delivery events**.

**Requires:** the `agent-deck session children` and `launch --assert-done`
features. If `agent-deck session children --help` succeeds, you have them.

## When to use

Use this when the user wants more than one agent working at once and wants to
supervise from the parent session — e.g. "launch 5 sessions to each tackle a
file", "fan out these tasks", "spin up a fleet and tell me when they're done".

This differs from the single sub-agent pattern in the `agent-deck` skill (one
child + fire-&-forget / on-demand / blocking retrieval). Fleet is **many
children + a non-blocking peek** across all of them.

Launches are roots unless you name the actual delegator with
`--parent <session-id>`. Fleet is for explicit delegated children; an
independent session is not a fleet child merely because it is launched nearby.
Use the long form because `-p` selects a profile, not a parent.

## Before you fan out

- **Check for shared singletons.** N sessions on the same project can't truly run
  in parallel if they serialize on one resource — a single dev DB, a bound port,
  one dev-server lock file. If they'd share one, either give each child isolated
  resources (own DB name + port) or run **one** coherent session instead of a
  fleet.
- **Worktree children need deps installed first.** A freshly created git worktree
  has no `node_modules` / vendored deps. Make the **first instruction** in the
  child's `-m` prompt install them — and for a locked monorepo, install *from the
  frozen lockfile, never regenerate it* (e.g. `pnpm install --frozen-lockfile`).
  Otherwise the child's first test/build/e2e fails confusingly.
- **Long prompts: pass via a file.** For a big multi-line task, write it to a
  file and pass `--message-file task.md` (or `--message-file -` to read stdin)
  instead of `-m`. The file is read directly by agent-deck, so backticks, `$`,
  and quotes never round-trip through the shell. Also works on
  `session start` and `session send` (there it replaces the positional
  message). On older builds without the flag, fall back to
  `-m "$(cat task.md)"`.

## The loop

### 1. Fan out (one `launch` per child; loop it)

```bash
agent-deck launch <path> -c claude --parent <actual-delegator-id> -m "<task for this child>"
```

- **Parentage is explicit.** Children appear under their actual delegator and
  inherit that session's group unless `--group` is explicitly set.
- **Groups organize the sidebar.** They neither select monitoring or recovery
  nor grant control over a session.
- **`--assert-done` is on by default for `-c claude`**: the child's message gets
  a final-step instruction to print the completion sentinel
  (`===AGENTDECK_DONE=== status=ok summary=…`) so "done" is trustworthy.
- Run it N times (different `<path>` and `-m` per child) to fan out a fleet.

Useful flags:
- `-t "<title>"` — give each child a readable title (otherwise auto-named).
- `--parent <id>` — explicitly parent the child to the actual delegator. Use
  the full session ID and long form.
- `--no-assert-done` — skip the completion-sentinel instruction.

### 2. Keep working

Nothing blocks. Do other work in this session, in any chat, while the fleet runs.

### 3. Check progress (non-blocking, non-destructive)

```bash
agent-deck session children --json
```

Lists your sub-sessions with, per child: `id`, `title`, live `status`
(running / waiting / idle / error), and the last asserted completion
(`done_status` = ok|fail, `done_summary`, `done_at`). Defaults to the current
session; pass an id/title to inspect another parent. **Read-only** — it never
clears the inbox, so you can poll it as often as you like from any chat without
disturbing the conductor or other readers.

A child with a `done_status` has finished and asserted its result.

**Prefer push over polling when your harness supports it.** Instead of
re-running the check yourself, let the fleet notify you:

```bash
# One-shot "wake me when the whole fleet is finished" — run this in the
# BACKGROUND (e.g. Claude Code's run_in_background Bash): it streams JSONL
# events and exits 0 once every child is terminal (done sentinel, error,
# or stopped). The harness notifies you when it exits.
agent-deck session children --follow --until-done

# Live event stream for a long-running fleet — attach a stream watcher
# (e.g. Claude Code's Monitor tool) to this; each line is one event:
agent-deck session children --follow
```

`--follow` emits one JSON object per line: `snapshot` (initial state per
child), `added`, `status` (from/to transition — including `running → waiting`,
so you see a child stall on a question), `done` (completion sentinel, ok or
fail), `removed`, `error`, plus a periodic `heartbeat` (default 60s,
`--heartbeat 0` disables) so silence always means "nothing changed", never
"the watcher died". `--interval` tunes the poll cadence (default 2s). Failure
states are on the stream too — filter for `done` alone and you'll miss
crashed children; key off `.event` instead.

On older builds without `--follow`, fall back to a background until-loop:

```bash
until agent-deck session children --json | jq -e 'all(.children[]; .done_status != null)' >/dev/null; do sleep 15; done
```

(Cloud-side schedulers — e.g. Claude Code routines — run on remote infra and
cannot reach your local tmux/state.db; fleet supervision stays local.)

### 4. Unblock a child that's waiting on you

A child in `waiting` status has stopped and is asking for input (a question, a
decision, a permission). This is **pushed to you by default**: a child's
`running → waiting` transition is delivered to your inbox unless it was launched
with `--no-transition-notify`. To answer it:

```bash
# See what the child is asking:
agent-deck session output <child-id> --json
# Send the answer (child keeps running afterward):
agent-deck session send <child-id> "<your answer>"

# Codex numbered approval prompt: send one decision key, not composer text:
agent-deck session approve <child-id> once
```

`session send` flags: `--wait` (block until it finishes the turn, then print
output), `--stream` (stream its JSONL events), `--no-wait` (fire and return),
`--draft` (pre-fill the prompt without submitting). Default waits only until the
child is ready to receive, then returns — so it does **not** freeze your session.

You can send follow-ups any time, not just when a child is waiting — e.g. to
add scope, redirect, or course-correct a still-running child.

### 5. Collect a finished child's result

```bash
agent-deck session output <child-id> --json
```

Returns that child's latest full response. Use it once `session children` shows
the child is done (or any time you want its current output).

## Worked example

```bash
# Fan out 3 children, each on a different package:
agent-deck launch ./pkg/a -c claude --parent <actual-delegator-id> -t "lint-a" -m "Fix all lint errors in this package."
agent-deck launch ./pkg/b -c claude --parent <actual-delegator-id> -t "lint-b" -m "Fix all lint errors in this package."
agent-deck launch ./pkg/c -c claude --parent <actual-delegator-id> -t "lint-c" -m "Fix all lint errors in this package."

# ...keep working, then whenever convenient:
agent-deck session children --json
#   → each child: status + done_status/done_summary

# If a child shows status "waiting", see its question and answer it:
agent-deck session output lint-a --json
agent-deck session send lint-a "Yes, drop the deprecated shim — don't keep a fallback."

# For each child reporting done, pull its result:
agent-deck session output lint-a --json
```

## Supervision tools the parent can use

All read-only / on-demand — none of them block your session:

- `agent-deck session children [id] --json` — **the default monitor.** Live
  status + last completion per child. Non-destructive (never clears the inbox),
  so poll it as often as you like. Start here every heartbeat.
- `agent-deck session children --follow [--until-done]` — **the push monitor.**
  Streams JSONL child events (snapshot/added/status/done/removed/error +
  heartbeat) until interrupted; with `--until-done` it exits 0 once every child
  is terminal. Run it in the background for a completion wake-up, or attach a
  stream watcher for live events. Read-only like the plain form.
- `agent-deck session output <id> --json` — a child's latest full response.
- `agent-deck session send <id> "<msg>" [--wait|--stream|--no-wait|--draft]` —
  send a follow-up / answer a `waiting` child.
- `agent-deck session approve <id> [once|always|session|N]` — resolve one
  visibly active Codex approval menu. Do not use `session send <id> "1"`:
  Codex consumes the digit as a decision key, while `session send` adds a
  trailing Enter that can land in the resumed turn.
- `agent-deck status -q` — global count of sessions currently `waiting`; a cheap
  coarse heartbeat across everything, not just your children.
- `agent-deck inbox drain --json <your-session-id>` — **consumes** the pushed
  completion events from your durable inbox (last-wins per child, deduped).
  Optional: `session children` already surfaces the same `done_status` without
  consuming anything, so only drain if you specifically want to clear the queue.
- `agent-deck session stop <id>` / `agent-deck session remove <id>` — teardown.

There is no always-on background watcher started for you — "monitored by
default" means transition/completion events **queue** in your inbox; you still
choose when to look (poll `session children`, or `inbox drain`).

**Automatic turn-start snapshot (Claude conductors, newer builds).** A Claude
parent session gets a compact fleet snapshot injected as context on every
prompt submit and session start — child counts plus actionable bullets for
`waiting` (with the exact `session output`/`session send` commands) and
completed children. This is *state*, complementing the Stop-edge inbox drain
(*events*): it survives conductor restarts and works even if events were
drained elsewhere. Leaf sessions see nothing. Opt a session out by launching
it with `AGENTDECK_NO_CHILDREN_CONTEXT=1` in its environment.

## Notes

- **Completion signal:** trustworthy "done" comes from the child printing
  `===AGENTDECK_DONE=== status=<ok|fail> summary=<one line>` as its last line.
  `--assert-done` (default-on for Claude) bakes this into the child's prompt; a
  child that never prints it shows live status but no `done_status`.
- **Non-blocking by design:** there is intentionally no "wait until all finish"
  command — checking is a cheap, repeatable query so a parent's other chats are
  never frozen.
- **Grouping:** a child inherits the actual parent's group. An explicit
  `--group` wins. Groups organize the sidebar only; use exact session IDs for
  monitoring and recovery selection.
- **The `-p` pitfall — use `--parent`, never `-p`, for a parent.** `-p` is the
  *global* `--profile` shorthand, parsed before the subcommand. On older builds it
  swallows your intended parent id as a profile name and routes the child into a
  phantom `~/.agent-deck/profiles/<id>/state.db` — the child runs in tmux but is
  invisible to the TUI / `ls` / `session children` (which read the default
  profile), and a retry then fails with "session already exists". The long-form
  `--parent <id>` is never affected and is the one-step way to set an explicit
  parent. If you already launched a child and need to (re)parent it after the
  fact, `agent-deck session set-parent <id|title> <parent-id>` also works.
  To clean up phantom DBs from a past `-p` slip: the orphaned rows live under
  `profiles/<parent-id>/state.db`; back up and remove that dir (the child's
  worktree/branch stay on disk).
- **Stopping / cleanup:** `agent-deck session stop <id>` and
  `agent-deck session remove <id>` (add `--force` if needed) tear a child down.
