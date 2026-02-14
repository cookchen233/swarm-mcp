# Swarm MCP (Issue-Centric)

A local MCP server (stdio) for multi-window, multi-agent collaboration, usable from any MCP-compatible host/client.

**English** | [中文](README.md)

This project implements an **issue-centric** workflow:

- **Issue**: the main problem (a shared pool container)
- **Task**: a claimable work unit under an issue (not assigned to a fixed worker)
- Collaboration happens via an **event stream** (long-poll)
- File changes are coordinated with **lease-based file locks**
- Context is shared through a **docs library**

## Features

- Issue pool: disseminate work as a pool, workers claim tasks freely
- Worker identity registration for traceability
- Event stream:
  - `waitIssueTaskEvents`: issue-level select-like long-poll
- Blocking Q&A: `askIssueTask` (worker blocks until lead replies)
- Task state linkage:
  - `kind=question|blocker` => task auto-transitions to `blocked`
  - `kind=reply` => task auto-transitions back to `in_progress`
- Strict mode (default): hide non-blocking ask primitive in `tools/list` so workers prefer blocking ask

## Design Notes

### Core Decisions

- **Deployment**: a local MCP server (stdio) launched by an MCP host; all windows share the same data root (`SWARM_MCP_ROOT`)
- **Collaboration channel**: Issue/Task event stream (lead runs a select-like loop via `waitIssueTaskEvents`)
- **Concurrency control**: lease-based file locks (`lockFiles` + `heartbeat` + `unlock`)
- **Persistence**: filesystem (default `~/.swarm-mcp/`)

### Task State Machine

```text
open -> in_progress -> submitted -> done
                   -> blocked
                   -> canceled
```

Message linkage:

- `askIssueTask(kind=question|blocker)` or `postIssueTaskMessage(kind=question|blocker)` auto-transitions the task to `blocked`
- lead replies via `replyIssueTaskMessage`, then the task auto-transitions back to `in_progress`

### File Lock Semantics (Must Understand)

- **Lease-based**: default TTL is 120s; call `heartbeat` periodically (e.g. every 30s)
- **Atomic multi-file locking**: `lockFiles(files=[...])` is all-or-nothing
- **Cross-process safety**: writes are guarded by a global lock file (`$SWARM_MCP_ROOT/.global.lock`)
- **Expired takeover**: after lease expiry, other windows can acquire the lock; audit records are emitted

### Strict Mode

- Enabled by default: `SWARM_MCP_STRICT != "0"`
- In strict mode: `tools/list` **hides** `postIssueTaskMessage`, so workers are guided to use blocking `askIssueTask`
- Note: current behavior is “hide”, not “hard reject”. (Hard reject can be added if you need stronger enforcement.)

### IDs / Session Context

- MCP tool calls are explicit-parameter by default; this server does not keep an implicit “current issue/task context”.
- If you forget IDs, recover by:
  - `listIssues` / `getIssue`
  - `listIssueTasks(issue_id)` / `getIssueTask`

In addition, this server introduces a **strongly required `session_id`** as a window/session isolation token (cookie-like semantics):

- Each window MUST call `openSession` first to obtain its own `session_id`.
- After that, **every `tools/call` MUST include `session_id`** (otherwise the server returns an error).

### Docs Library: shared vs issue vs task

- **Shared docs (global)**: documents shared across all issues
  - On disk: `$SWARM_MCP_ROOT/docs/shared/<name>.md`
  - Tools: `writeSharedDoc` / `readSharedDoc` / `listSharedDocs`
- **Issue docs (per issue)**: specs/decisions/context for a single issue
  - On disk: `$SWARM_MCP_ROOT/issues/<issue_id>/docs/<name>.md`
  - Tools: `writeIssueDoc` / `readIssueDoc` / `listIssueDocs`
- **Task docs (per task)**: optional task-specific materials/deliverables
  - On disk: `$SWARM_MCP_ROOT/issues/<issue_id>/tasks/<task_id>/docs/<name>.md`
  - Tools: `writeTaskDoc` / `readTaskDoc` / `listTaskDocs`

Recommendations:

- If you want to avoid maintaining global docs, use issue docs only.
- If you want a stable “team standard” shared across issues, use shared docs.

### Two-Phase Injection (Recommended)

```text
Phase 1 (free analysis)
- Do not mention MCP, do not call tools
- Clarify goals, risks, plan, and validation

Phase 2 (collaboration)
- Enter collaboration: createIssue / createIssueTask
- From now on: lockFiles before editing; heartbeat while holding; unlock when done
```

#### Phase 2: Prompts and Minimal Flow (Copy/Paste Ready)

Below are ready-to-copy **lead / worker prompt templates** for your MCP host/client, plus a minimal end-to-end flow.

##### Lead Prompt

```text
Now please write the development checklist into a document, for subsequent acceptance.

Then enter collaboration phase: I have a collaboration MCP Server called swarm-mcp.
You need to:
1) Based on your previous analysis, re-plan once (you may re-split / re-order) to better fit multi-agent collaboration.
2) Then use swarm-mcp to autonomously complete: create the issue, split tasks, and disseminate the issue so workers can claim tasks by themselves.

[Role]
You are the Lead.

[Collaboration rules]
- You MUST call openSession first to obtain session_id. All tool calls MUST include this session_id.
- First check if there is an existing issue with almost the same subject that is not closed; if so, close it and recreate.
- Preferred flow: createIssue -> createIssueTask -> waitIssueTaskEvents -> review/reply.
- Unless explicitly requested: you must act as a lead only (split tasks / answer questions / review / event loop). Do not implement tasks yourself and do not edit the workers' target code files.
- Q&A: workers use askIssueTask; you MUST reply using replyIssueTaskMessage with the same issue_id/task_id.
- Issues have a lease timeout. Based on createIssue/getIssue lease fields, call extendIssueLease in time (use swarmNow for server time).
- **Strong constraint**: when waitIssueTaskEvents returns a signal, you MUST reason carefully and handle it.
- **Strong constraint**: after handling, keep calling waitIssueTaskEvents until all tasks are completed and you call closeIssue.
```

##### Worker Prompt

```text
You are currently in MCP collaboration mode and can call the tools provided by swarm-mcp to complete tasks.
If you cannot see swarm-mcp tools in your MCP host: first try tools/list.

[Role]
You are a Worker.

[Collaboration rules]
- You MUST call openSession first to obtain session_id. All tool calls MUST include this session_id.
- Claim tasks: listIssueTasks -> find open -> claimIssueTask.
- When you see no open issues or tasks, call waitIssues or waitIssueTasks immediately.
- Before starting, read context: prioritize task doc_paths / required_*_docs via readIssueDoc / readTaskDoc.
- Before editing: lockFiles(files=[...]). Without a valid lockFiles lease, do not modify any file. While holding locks, heartbeat every ~30s. After finishing, unlock.
- If blocked/uncertain due to missing info: ask the lead using askIssueTask(kind=question|blocker) and wait for reply.
- Tasks have a lease timeout. Based on claimIssueTask/getIssueTask lease fields, call extendIssueTaskLease in time (use swarmNow for server time).
- **Strong constraint**: after finishing work, submit via submitIssueTask; continue with the next step according to next_actions returned by submitIssueTask.
- **Strong constraint**: when all tasks are completed, keep calling waitIssues or waitIssueTasks.
```

##### Minimal End-to-End Flow

###### Blocking / Long-Poll Semantics (Quick Reference)

Some tools are intentionally blocking (hanging) to support passive event loops and strict collaboration.

| Tool | Blocks? | Returns when | Default / fixed timeout | Notes |
| --- | --- | --- | --- | --- |
| `waitIssueTaskEvents(issue_id)` | Yes | Signals only: `question/blocker` or `issue_task_submitted` | Fixed 600s | Lead passive loop; returns at most 1 signal event per call; ignores other events and keeps hanging |
| `submitIssueTask(issue_id, task_id, ...)` | Yes | After submitting, until lead review produces `reviewed/resolved` events | Fixed 600s | Submission must include structured `artifacts`; prevents workers from exiting immediately after submitting |
| `askIssueTask(issue_id, task_id, ...)` | Yes | Lead replies via `replyIssueTaskMessage` | Default 600s (configurable) | Posts `question/blocker` first, then waits for reply |
| `lockFiles(...)` | Sometimes | Returns when lock acquired; waits up to `wait_sec` if busy | `wait_sec` | Not infinite; fails on timeout |

Note: task IDs are issue-local sequential IDs: `task-1`, `task-2`, ... (no conflicts across issues).

1. **Lead creates the work pool**
   - `createIssue(subject="...", description="...", user_issue_doc={name:"user", content:"..."}, lead_issue_doc={name:"lead", content:"..."}, user_other_docs=[{name:"context", content:"..."}])`
   - `createIssueTask(issue_id, subject="...", description="...", difficulty="easy|medium|focus", context_task_ids=[...], suggested_files=[...], spec={name:"spec", split_from:"...", split_reason:"...", impact_scope:"...", context_task_ids:[...], goal:"...", rules:"...", constraints:"...", conventions:"...", acceptance:"..."})`

2. **Worker claims and implements**
   - (Optional) If the lead has not created any issues yet, call `waitIssues(after_count=0, timeout_sec=600)` to block until an issue exists
   - (Optional) If you already know `issue_id` but the lead has not created any tasks yet, call `waitIssueTasks(issue_id, after_count=0, timeout_sec=600)` to block until a task exists
   - `listIssueTasks(issue_id)` -> find an `open` task
   - `claimIssueTask(issue_id, task_id)` (if the task is reserved by lead, you MUST provide `next_step_token`)
   - `lockFiles(task_id, files=["path/to/file.go"], ttl_sec=120, wait_sec=60)`
   - (implement changes; `heartbeat(lease_id)` while holding)
   - `unlock(lease_id)`
   - `submitIssueTask(issue_id, task_id, artifacts={summary:"...", changed_files:[...], diff:"...", links:[...]})` -> blocks until lead review

   The worker should run this as a loop:

   - Do not end the conversation after `submitIssueTask` returns. The response includes `next_actions`; continue by executing those actions (e.g. `listIssueTasks` / `claimIssueTask`) to pick up the next work item.
   - Repeat “claim -> lock -> implement -> submit” until:
     - `listIssueTasks(issue_id, status="open")` is empty (no claimable tasks), or
     - the lead explicitly ends the issue / stops dispatching tasks.

3. **Lead reviews or rejects**
   - `waitIssueTaskEvents(issue_id)` -> receive submitted/question/blocker
   - `getNextStepToken(issue_id, task_id, worker_id, completion_score=1|2|5)` -> server auto-picks and reserves the next task (or end), returns `next_step_token`
   - `reviewIssueTask(issue_id, task_id, verdict="approved|rejected", feedback="...", completion_score=1|2|5, artifacts={review_summary:"...", reviewed_refs:[...]}, feedback_details=[...], next_step_token="...")`

4. **Q&A (recommended in strict mode)**
   - Worker: `askIssueTask(issue_id, task_id, kind="question", content="...", timeout_sec=600)`
   - Lead: `replyIssueTaskMessage(issue_id, task_id, content="...")`

## Build & Run

### Build

```bash
go build -o bin/swarm-mcp ./cmd/swarm-mcp/
```

### Run (stdio)

This server is typically launched and managed by an MCP host as an MCP stdio server.

## MCP Client Configuration

Add this server to your MCP host/client configuration. The file location and schema depend on the client.

Add the server config:

```json
{
  "mcpServers": {
    "swarm-mcp": {
      "command": "/ABS/PATH/TO/swarm-mcp/bin/swarm-mcp",
      "args": [],
      "env": {
        "SWARM_MCP_ROOT": "/Users/you/.swarm-mcp/<project_key>",
        "SWARM_MCP_STRICT": "1"
      }
    }
  }
}
```

### Environment Variables

- `SWARM_MCP_ROOT`
  - Data directory root
  - **Default**: `$HOME/.swarm-mcp`
  - Recommended: isolate per project, e.g. `~/.swarm-mcp/<project_key>`
- `SWARM_MCP_STRICT`
  - Strict tool exposure switch
  - **Default**: enabled (any value other than `0`)
  - Set to `0` to return full tool list (including `postIssueTaskMessage`)

## Data Directory Layout

Default root: `~/.swarm-mcp/` (or `SWARM_MCP_ROOT`)

```text
$SWARM_MCP_ROOT/
  .global.lock
  docs/
    shared/
      <name>.md
  issues/
    <issue_id>/
      issue.json
      meta.json
      events.jsonl
      tasks/
        <task_id>.json
      docs/
        <name>.md
  workers/
    <worker_id>.json
  locks/
    files/
      <path_hash>.json
    leases/
      <lease_id>.json
  trace/
    events.jsonl
```

## Recommended Collaboration Workflow

### Lead window

1. `createIssue`
2. `createIssueTask` (split work into parallelizable tasks)
3. `waitIssueTaskEvents` (select-like loop)
4. `replyIssueTaskMessage` / `reviewIssueTask`

### Worker window

1. `listIssueTasks` -> find an `open` task
2. `claimIssueTask`
3. `lockFiles` (always lock before editing)
4. Implement changes (`heartbeat` while holding locks)
5. `unlock`
6. `submitIssueTask`

### Worker Q&A (preferred in strict mode)

- Use `askIssueTask(kind=question|blocker, ...)`
  - The call blocks until the lead uses `replyIssueTaskMessage`

## Manual Verification (Recommended)

### Step 0: Configure MCP Server

See “MCP Client Configuration” above. Recommended settings:

- `SWARM_MCP_ROOT=~/.swarm-mcp/<project_key>` (isolate per project)
- `SWARM_MCP_STRICT=1` (strict by default)
- `SWARM_MCP_SUGGESTED_MIN_TASK_COUNT`: suggested minimum task count
- `SWARM_MCP_MAX_TASK_COUNT`: maximum tasks allowed per issue (enforced at `createIssueTask`; rejects when exceeded)
- `SWARM_MCP_ISSUE_TTL_SEC=3600`: issue lease TTL (auto-canceled as `canceled` when expired)
- `SWARM_MCP_TASK_TTL_SEC=600`: task lease TTL (auto-reopened to `open` from `in_progress/blocked/submitted` when expired)

Restart your MCP host/client and ensure swarm-mcp tools show up.

### Step 1: Open Two Windows

- **Window A (Lead)**: split work, answer questions, review/accept (usually no direct code edits)
- **Window B (Worker)**: claim tasks, lock files, implement, submit

Each window has its own `member_id` (via `whoAmI`) for audit/traceability.

### Step 2: Lead Creates the Issue and Tasks

- `createIssue`
- (optional) `extendIssueLease` (extend issue lease to avoid auto-cancel on inactivity)
- multiple `createIssueTask` (must set `difficulty=easy|medium|focus`)
- start a `waitIssueTaskEvents` loop

### Step 3: Worker Claims and Implements

- `listIssueTasks` -> pick an `open` task
- `claimIssueTask`
- (optional) `extendIssueTaskLease` (extend task lease to avoid it being auto-reopened to `open`)
- `lockFiles` (lock the files for this task)
- implement changes (`heartbeat` while holding)
- `unlock`
- `submitIssueTask`

### Step 4: Lead Reviews

- `waitIssueTaskEvents` to receive submitted events
- `getNextStepToken` to mint a typed next step token (server auto-assigns + reserves; `end` or `claim_task`)
- `reviewIssueTask` (must include `completion_score`, `feedback_details`, `artifacts`, and `next_step_token`)

### Step 5: Q&A / Blocking (Validate strict + ask)

- worker: `askIssueTask(kind=question|blocker, ...)` (blocks)
- lead: `replyIssueTaskMessage(...)`
- observe status transitions: `in_progress -> blocked -> in_progress`

## Key Scenarios (Avoid Production Surprises)

### Lock Conflicts

1. worker locks a file via `lockFiles`
2. lead/another worker immediately locks the same file (`wait_sec=0`) should fail

### Expired Lease Takeover

1. worker locks files with a small `ttl_sec`, without heartbeats
2. after expiry, another window should be able to lock the same file
3. check `$SWARM_MCP_ROOT/trace/events.jsonl` for expiry-related audit records

### Atomic Multi-File Locks

1. `lockFiles(files=["a.go","b.go"])`
2. another window attempts a set involving `b.go` and should fail
3. after unlock, retry should succeed

## Audit Log

All operations are recorded at:

- `$SWARM_MCP_ROOT/trace/events.jsonl`

Quick grep examples:

```bash
grep lock "$SWARM_MCP_ROOT/trace/events.jsonl"
```

## Issue/Task Auto-Expiration (Resilience)

This project does not rely on a background scheduler. Expiration handling is triggered synchronously on common tool entrypoints.

### When does expiration run?

Whenever a window calls any of the following tools, the server will perform a best-effort expiration sweep:

- `listIssues`
- `waitIssues`
- `getIssue`
- `closeIssue`
- `waitIssueTasks`
- `listIssueTasks`
- `getIssueTask`
- `claimIssueTask`
- `submitIssueTask`
- `waitIssueTaskEvents`

### What happens on expiration?

- Expired issue: `open|in_progress` -> `canceled`, and append `issue_expired`
- Expired task: `in_progress|blocked|submitted` -> `open` (reclaimable), and append `issue_task_expired`

### Response fields (how to know when to extend)

All tools that return an issue/task (e.g. `createIssue/getIssue/listIssues`, `createIssueTask/getIssueTask/listIssueTasks`, `claimIssueTask`, `extendIssueLease/extendIssueTaskLease`) include:

- `lease_expires_at_ms`: expiry timestamp (unix ms)
- `lease_expires_at`: expiry timestamp (RFC3339, UTC)
- `server_now_ms`: server current time (unix ms)
- `server_now`: server current time (RFC3339, UTC)

### Clock alignment

- Use `swarmNow` to query server time: returns `now_ms` and `now` (RFC3339, UTC)

## Cleanup (Start Fresh)

```bash
trash "$SWARM_MCP_ROOT"
```

## Troubleshooting

### 1) I cannot see swarm-mcp tools in my MCP host/client

- **Check the MCP config path**: follow your MCP host/client documentation
- **Check the command path**: it should point to an executable like `bin/swarm-mcp`
- **Restart the MCP host/client**: stdio MCP servers are typically relaunched on restart

### 2) Why is `postIssueTaskMessage` missing?

- In strict mode it is **hidden** from `tools/list` (not removed).
- The preferred worker ask path is `askIssueTask` (blocks until reply).
- To expose the full tool list: set `SWARM_MCP_STRICT=0`.

### 3) `askIssueTask` blocks forever / times out

- Expected behavior: it blocks until the lead calls `replyIssueTaskMessage`.
- If it times out:
  - ensure the lead replied to the same `issue_id/task_id`
  - ensure both windows are using the same `SWARM_MCP_ROOT` (easy to misconfigure when isolating projects)

### 4) `waitIssueTaskEvents` returns nothing

- It is a long-poll API:
  - if there are no new events, they return an empty array after `timeout_sec`
  - the intended usage is cursor-based: `after_seq` / `next_seq`

Recommended pattern:

- first call: `after_seq=0`
- then: update `after_seq` to the returned `next_seq`

### 5) Lock issues (lockFiles fails / looks like a deadlock)

- `lockFiles` failure usually means:
  - the file is locked by another window
  - or the wait timed out (`wait_sec`)
- Debug with:
  - `listLocks` to inspect owner and expiry
  - `$SWARM_MCP_ROOT/trace/events.jsonl` for audit events (e.g. expiry/force unlock)

### 6) Why do two windows “block each other”: lead is waiting, and worker calls like listIssueTasks also look hung

- **Root cause (historical)**: if lead/worker share one stdio server process and the server processes requests synchronously, a long-poll call can block other calls.
- **Current implementation**: the server now handles requests concurrently and uses a strongly required `session_id` to isolate windows.
- **Correct usage**: call `openSession` per window and include `session_id` in every `tools/call`.
- **Debugging**: if you see `session_id is required` or `unknown session_id`, the window did not call `openSession` first, or is using the wrong session_id.

## Key Tools (Summary)

- Issue / Task
  - `createIssue`, `listIssues`, `getIssue`
  - `createIssueTask`, `listIssueTasks`, `getIssueTask`
  - `claimIssueTask`, `submitIssueTask`, `reviewIssueTask`
  - `waitIssueTaskEvents`
  - `askIssueTask`, `replyIssueTaskMessage`
- Docs
  - `writeSharedDoc`, `readSharedDoc`, `listSharedDocs`
  - `writeIssueDoc`, `readIssueDoc`, `listIssueDocs`
  - `writeTaskDoc`, `readTaskDoc`, `listTaskDocs`
- Worker
  - `registerWorker`, `listWorkers`, `getWorker`, `whoAmI`
- Locks
  - `lockFiles`, `heartbeat`, `unlock`, `listLocks`, `forceUnlock`

## Tests

```bash
go test ./...

bash test.sh
```

## License

TBD
