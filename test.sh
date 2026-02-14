#!/bin/bash
# Swarm MCP - Automated Integration Test Script
# Tests all MCP tools via stdin/stdout JSON-RPC protocol.

set -e

BIN="$(dirname "$0")/bin/swarm-mcp"
export SWARM_MCP_ROOT="/tmp/swarm-mcp-test-$$"
PASS=0
FAIL=0

# Ensure we test the latest code.
mkdir -p "$(dirname "$0")/bin"
go build -o "$BIN" ./cmd/swarm-mcp/ >/dev/null 2>&1 || go build -o "$BIN" ./cmd/swarm-mcp/

mkdir -p "$SWARM_MCP_ROOT"

cleanup() {
    if [ -n "${RESP_READER_PID:-}" ]; then
        kill "$RESP_READER_PID" >/dev/null 2>&1 || true
    fi
    if [ -n "${SWARM_MCP_PROC_PID:-}" ]; then
        kill "$SWARM_MCP_PROC_PID" >/dev/null 2>&1 || true
    fi
    trash "$SWARM_MCP_ROOT" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Persistent stdio server process (required for session_id mapping).
# Note: macOS ships bash 3.2 by default, which does not support `coproc`.
FIFO_IN="$SWARM_MCP_ROOT/mcp.in"
FIFO_OUT="$SWARM_MCP_ROOT/mcp.out"
mkfifo "$FIFO_IN" "$FIFO_OUT"

"$BIN" <"$FIFO_IN" >"$FIFO_OUT" 2>/dev/null &
SWARM_MCP_PROC_PID=$!

# Stable FDs for writing/reading JSON-RPC lines.
exec 3>"$FIFO_IN"
exec 4<"$FIFO_OUT"
MCP_IN_FD=3
MCP_OUT_FD=4

RESP_DIR="$SWARM_MCP_ROOT/responses"
mkdir -p "$RESP_DIR"

(
    while IFS= read -r line <&$MCP_OUT_FD; do
        id=$(echo "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
        if [ -n "$id" ]; then
            echo "$line" > "$RESP_DIR/$id.json"
        fi
    done
) &
RESP_READER_PID=$!

call() {
    local id="$1"
    local method="$2"
    local params="$3"
    trash "$RESP_DIR/$id.json" >/dev/null 2>&1 || true
    echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"method\":\"$method\",\"params\":$params}" >&$MCP_IN_FD
    for _ in $(seq 1 500); do
        if [ -f "$RESP_DIR/$id.json" ]; then
            cat "$RESP_DIR/$id.json"
            return 0
        fi
        sleep 0.01
    done
    echo "TIMEOUT waiting for response id=$id"
    return 1
}

call_async() {
    local id="$1"
    local method="$2"
    local params="$3"
    trash "$RESP_DIR/$id.json" >/dev/null 2>&1 || true
    echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"method\":\"$method\",\"params\":$params}" >&$MCP_IN_FD
}

wait_resp() {
    local id="$1"
    for _ in $(seq 1 1000); do
        if [ -f "$RESP_DIR/$id.json" ]; then
            cat "$RESP_DIR/$id.json"
            return 0
        fi
        sleep 0.01
    done
    echo "TIMEOUT waiting for response id=$id"
    return 1
}

with_session() {
    local sid="$1"
    local args="$2"
    if [ "$args" = "{}" ]; then
        echo "{\"session_id\":\"$sid\"}"
        return 0
    fi
    echo "$args" | sed "s/^{/{\\\"session_id\\\":\\\"$sid\\\",/"
}

tool_call() {
    local id="$1"
    local tool="$2"
    local sid="$3"
    local args="$4"
    local merged
    merged=$(with_session "$sid" "$args")
    call "$id" "tools/call" "{\"name\":\"$tool\",\"arguments\":$merged}"
}

tool_call_async() {
    local id="$1"
    local tool="$2"
    local sid="$3"
    local args="$4"
    local merged
    merged=$(with_session "$sid" "$args")
    call_async "$id" "tools/call" "{\"name\":\"$tool\",\"arguments\":$merged}"
}

# Helper: check if response contains expected text
assert_contains() {
    local desc="$1"
    local response="$2"
    local expected="$3"
    if echo "$response" | grep -Fq "$expected"; then
        PASS=$((PASS + 1))
        echo "  ✓ $desc"
    else
        FAIL=$((FAIL + 1))
        echo "  ✗ $desc"
        echo "    Expected to contain: $expected"
        echo "    Got: $response"
    fi
}

assert_not_contains() {
    local desc="$1"
    local response="$2"
    local unexpected="$3"
    if echo "$response" | grep -Fq "$unexpected"; then
        FAIL=$((FAIL + 1))
        echo "  ✗ $desc"
        echo "    Should NOT contain: $unexpected"
        echo "    Got: $response"
    else
        PASS=$((PASS + 1))
        echo "  ✓ $desc"
    fi
}

echo "=== Swarm MCP Integration Tests ==="
echo "Data root: $SWARM_MCP_ROOT"
echo ""

# --- Test 1: Initialize ---
echo "[Test 1] Initialize"
resp=$(call 1 "initialize" '{}')
assert_contains "returns server info" "$resp" "swarm-mcp"
assert_contains "returns protocol version" "$resp" "2024-11-05"
echo ""

echo "[Test 1] Open Session"
resp=$(call 1 "tools/call" "{\"name\":\"openSession\",\"arguments\":{}}")
assert_not_contains "no error" "$resp" "isError"
LEAD_SESSION=$(echo "$resp" | grep -oE 'sess_[0-9]+_[0-9a-f]+' | head -1)
echo "  (lead_session=$LEAD_SESSION)"
echo ""

echo "[Test 1.1] Server now"
resp=$(tool_call 101 "swarmNow" "$LEAD_SESSION" "{}")
assert_contains "now has ms" "$resp" "now_ms"
assert_contains "now has rfc3339" "$resp" "now"
echo ""

echo "[Setup] Open sessions"
resp=$(tool_call 2 "openSession" "" '{}')
assert_contains "opens lead session" "$resp" "session_id"
LEAD_SESSION=$(echo "$resp" | grep -oE 'sess_[0-9]+_[0-9a-f]+' | head -1)
assert_contains "opens lead session" "$resp" "$LEAD_SESSION"
resp=$(tool_call 91 "openSession" "" '{}')
assert_contains "opens worker session" "$resp" "session_id"
WORKER_SESSION=$(echo "$resp" | grep -oE 'sess_[0-9]+_[0-9a-f]+' | head -1)
assert_contains "opens worker session" "$resp" "$WORKER_SESSION"
echo "  (lead_session=$LEAD_SESSION)"
echo "  (worker_session=$WORKER_SESSION)"
echo ""

# --- Test 2.1: Worker can wait for issues before lead creates any ---
echo "[Test 2.1] Wait Issues (blocks until issue exists)"
tool_call_async 92 "waitIssues" "$WORKER_SESSION" '{"after_count":0,"timeout_sec":5}'
sleep 1

# --- Test 2: Tools List ---
echo "[Test 2] Tools List"
resp=$(call 3 "tools/list" '{}')
assert_contains "lists lockFiles tool" "$resp" "lockFiles"
assert_contains "lists createIssue tool" "$resp" "createIssue"
assert_contains "lists createIssueTask tool" "$resp" "createIssueTask"
assert_contains "lists waitIssueTaskEvents tool" "$resp" "waitIssueTaskEvents"
assert_contains "lists docs tools" "$resp" "writeSharedDoc"
assert_contains "lists worker tools" "$resp" "registerWorker"
assert_contains "lists issue pool tools" "$resp" "listIssues"
assert_contains "lists askIssueTask tool" "$resp" "askIssueTask"
assert_not_contains "strict mode hides postIssueTaskMessage" "$resp" "postIssueTaskMessage"
echo ""

# --- Test 3: Create Issue ---
echo "[Test 3] Create Issue"
resp=$(tool_call 4 "createIssue" "$LEAD_SESSION" '{"subject":"Test Issue","description":"For integration tests","shared_doc_paths":["docs/shared/spec.md"],"project_doc_paths":["README.md"],"user_issue_doc":{"name":"user_issue","content":"User provided context"},"lead_issue_doc":{"name":"lead_issue","content":"Lead refined context"},"user_other_docs":[{"name":"api_docs","content":"Optional Docs"}]}' )
assert_contains "creates issue" "$resp" "issue_"
assert_contains "issue has lease_expires_at" "$resp" "lease_expires_at"
assert_contains "issue has server_now" "$resp" "server_now"
ISSUE_ID=$(echo "$resp" | grep -oE 'issue_[0-9]+_[0-9a-f]+' | head -1)
if [ -z "$ISSUE_ID" ]; then
    echo "FAILED to parse ISSUE_ID from response: $resp"
    exit 1
fi
echo "  (issue_id=$ISSUE_ID)"

WAIT_ISSUES_RESP=$(wait_resp 92)
assert_contains "waitIssues returns issue" "$WAIT_ISSUES_RESP" "$ISSUE_ID"
echo ""

# --- Test 3.1: Worker can wait for tasks before lead creates any ---
echo "[Test 3.1] Wait Issue Tasks (blocks until task exists)"
tool_call_async 93 "waitIssueTasks" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"after_count\":0,\"timeout_sec\":5}"
sleep 1

# --- Test 4: Issue pool listing ---
echo "[Test 4] List Issues"
resp=$(tool_call 4 "listIssues" "$WORKER_SESSION" '{}')
assert_contains "lists issues" "$resp" "$ISSUE_ID"
echo ""

# --- Test 5: Get Issue ---
echo "[Test 5] Get Issue"
resp=$(tool_call 5 "getIssue" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\"}")
assert_contains "gets issue" "$resp" "shared_doc_paths"
assert_contains "gets project doc paths" "$resp" "project_doc_paths"
assert_contains "issue has lease_expires_at" "$resp" "lease_expires_at"
assert_contains "issue has server_now" "$resp" "server_now"
echo ""

# --- Test 6: Update Issue Doc Paths ---
echo "[Test 6] Update Issue Doc Paths"
resp=$(tool_call 6 "updateIssueDocPaths" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"project_doc_paths\":[\"README.md\",\"docs/PROJECT.md\"]}")
assert_contains "updates issue" "$resp" "docs/PROJECT.md"
echo ""

# --- Test 7: Docs shared write/read ---
echo "[Test 7] Docs Shared"
resp=$(tool_call 7 "writeSharedDoc" "$LEAD_SESSION" '{"name":"spec","content":"# Spec\nhello"}')
assert_contains "writes shared doc" "$resp" "docs"
resp=$(tool_call 8 "readSharedDoc" "$WORKER_SESSION" '{"name":"spec"}')
assert_contains "reads shared doc" "$resp" "# Spec"
echo ""

# --- Test 8: Create Issue Task ---
echo "[Test 8] Create Issue Task"
resp=$(tool_call 9 "createIssueTask" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"subject\":\"Implement auth module\",\"description\":\"DoD: compiles\",\"difficulty\":\"easy\",\"context_task_ids\":[],\"suggested_files\":[\"auth/handler.go\"],\"labels\":[\"backend\"],\"doc_paths\":[\"docs/shared/spec.md\",\"README.md\"],\"points\":5,\"spec\":{\"name\":\"spec\",\"split_from\":\"p0\",\"split_reason\":\"parallelize\",\"impact_scope\":\"auth module\",\"goal\":\"Implement auth module end-to-end\",\"rules\":\"Keep existing API stable\",\"constraints\":\"No breaking changes; keep latency acceptable\",\"conventions\":\"Follow existing naming and file layout\",\"acceptance\":\"Build passes; tests updated; endpoints behave as specified\"}}")
assert_contains "creates issue task" "$resp" "Implement auth module"
assert_contains "has points" "$resp" "\\\"points\\\": 5"
assert_contains "task has lease_expires_at" "$resp" "lease_expires_at"
assert_contains "task has server_now" "$resp" "server_now"
TASK_ID=$(echo "$resp" | grep -oE 'task-[0-9]+' | head -1)
if [ -z "$TASK_ID" ]; then
    echo "FAILED to parse TASK_ID from response: $resp"
    exit 1
fi
echo "  (task_id=$TASK_ID)"

WAIT_TASKS_RESP=$(wait_resp 93)
assert_contains "waitIssueTasks returns task" "$WAIT_TASKS_RESP" "$TASK_ID"

echo "[Test 8.1] Create Issue Task #2 (reserved claim target)"
resp=$(tool_call 9 "createIssueTask" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"subject\":\"Implement auth module (part 2)\",\"description\":\"DoD: compiles\",\"difficulty\":\"easy\",\"context_task_ids\":[\"$TASK_ID\"],\"suggested_files\":[\"auth/service.go\"],\"labels\":[\"backend\"],\"doc_paths\":[\"docs/shared/spec.md\",\"README.md\"],\"points\":3,\"spec\":{\"name\":\"spec\",\"split_from\":\"Auth module requirement (continuation)\",\"split_reason\":\"Separate service layer from handler layer for clarity\",\"impact_scope\":\"backend/auth service; may affect handler imports\",\"context_task_ids\":[\"$TASK_ID\"],\"goal\":\"Implement auth module (part 2)\",\"rules\":\"Keep existing API stable\",\"constraints\":\"No breaking changes\",\"conventions\":\"Follow existing naming\",\"acceptance\":\"Build passes\"}}")
TASK2_ID="task-2"
assert_contains "creates task-2" "$resp" "$TASK2_ID"
echo "  (task2_id=$TASK2_ID)"
echo ""

# --- Test 9: Claim Issue Task ---
echo "[Test 9] Claim Issue Task"
resp=$(tool_call 10 "claimIssueTask" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\"}")
assert_contains "status in_progress" "$resp" "in_progress"
assert_contains "claim has lease_expires_at" "$resp" "lease_expires_at"
assert_contains "claim has server_now" "$resp" "server_now"

# Worker must be able to read required issue docs and task spec.
resp=$(tool_call 10 "readIssueDoc" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"name\":\"user_issue\"}")
assert_contains "reads user_issue" "$resp" "User provided context"
resp=$(tool_call 10 "readIssueDoc" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"name\":\"lead_issue\"}")
assert_contains "reads lead_issue" "$resp" "Lead refined context"
resp=$(tool_call 10 "readIssueDoc" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"name\":\"api_docs\"}")
assert_contains "reads api_docs" "$resp" "Optional Docs"
resp=$(tool_call 10 "readTaskDoc" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\",\"name\":\"spec\"}")
assert_contains "reads task spec" "$resp" "# Spec"
assert_contains "spec contains goal" "$resp" "Implement auth module end-to-end"
echo ""

# --- Test 10: waitTaskEvents should NOT be exposed ---
echo "[Test 10] waitTaskEvents is not exposed"
resp=$(tool_call 11 "waitTaskEvents" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\",\"after_seq\":0,\"timeout_sec\":1,\"limit\":20}")
assert_contains "waitTaskEvents returns error" "$resp" "isError"
assert_contains "waitTaskEvents unknown tool" "$resp" "unknown tool"
echo ""

# --- Test 11: Worker registered ---
echo "[Test 11] List Workers"
resp=$(tool_call 12 "listWorkers" "$LEAD_SESSION" '{}')
assert_contains "has worker" "$resp" "\"id\""
echo ""

# --- Test 12: Ask Issue Task (blocks until reply) ---
echo "[Test 12] Ask Issue Task"

# Start ask in background and then reply.
tool_call_async 13 "askIssueTask" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\",\"kind\":\"question\",\"content\":\"Need decision: X or Y?\",\"timeout_sec\":5}"

# Give it a moment to post question + block
sleep 1

# Task should be blocked after question
resp=$(tool_call 14 "getIssueTask" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\"}")
assert_contains "task blocked" "$resp" "\\\"status\\\": \\\"blocked\\\""

# Lead replies
resp=$(tool_call 15 "replyIssueTaskMessage" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\",\"content\":\"Choose X\"}")
assert_contains "replied" "$resp" "reply"

ASK_RESP=$(wait_resp 13)
assert_contains "ask returns reply" "$ASK_RESP" "Choose X"

# Task should be unblocked back to in_progress
resp=$(tool_call 16 "getIssueTask" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\"}")
assert_contains "task unblocked" "$resp" "\\\"status\\\": \\\"in_progress\\\""
echo ""

# --- Test 13: Submit Issue Task (blocks until review) ---
echo "[Test 13] Submit Issue Task (blocking)"
tool_call_async 17 "submitIssueTask" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\",\"artifacts\":{\"summary\":\"Implemented feature\",\"changed_files\":[\"auth/handler.go\"],\"diff\":\"diff --git a/auth/handler.go b/auth/handler.go\n\" ,\"links\":[\"https://example.com/diff\"],\"test_cases\":[\"go test ./...\"],\"test_result\":\"PASS\",\"test_output\":\"ok\\tmodule\\t0.01s\"}}"

echo "[Test 14] Wait Issue Task Events"
# waitIssueTaskEvents returns exactly one signal event per call.
# It may return an earlier question/blocker signal first, so we keep waiting until we see submitted.
resp=""
for _ in $(seq 1 10); do
    resp=$(tool_call 18 "waitIssueTaskEvents" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\"}")
    if echo "$resp" | grep -Fq "issue_task_submitted"; then
        break
    fi
done
assert_contains "returns submitted signal" "$resp" "issue_task_submitted"
echo ""

echo "[Test 15] Review Issue Task"
echo "[Test 15.1] Get Next Step Token"
# Derive worker_id from task submitter.
resp=$(tool_call 19 "getIssueTask" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\"}")
WORKER_ID=$(echo "$resp" | grep -oE 'm_[0-9]+_[0-9a-f]+' | head -1)
assert_contains "task has submitter" "$resp" "$WORKER_ID"

resp=$(tool_call 19 "getNextStepToken" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\",\"worker_id\":\"$WORKER_ID\",\"completion_score\":5}")
NEXT_STEP_TOKEN=$(echo "$resp" | grep -oE 'ns_[0-9]+_[0-9a-f]+' | head -1)
assert_contains "mints next_step_token" "$resp" "$NEXT_STEP_TOKEN"

echo "[Test 15.1.1] Reserved task cannot be claimed without token"
resp=$(tool_call 19 "claimIssueTask" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK2_ID\"}")
assert_contains "reserved claim rejected" "$resp" "reserved"

echo "[Test 15.2] Review Issue Task"
resp=$(tool_call 19 "reviewIssueTask" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\",\"verdict\":\"approved\",\"feedback\":\"LGTM\",\"completion_score\":5,\"artifacts\":{\"review_summary\":\"Reviewed diff + tests\",\"reviewed_refs\":[\"auth/handler.go\",\"https://example.com/diff\"]},\"feedback_details\":[{\"dimension\":\"correctness\",\"severity\":\"info\",\"content\":\"OK\"}],\"next_step_token\":\"$NEXT_STEP_TOKEN\"}")
assert_contains "status done" "$resp" "done"

echo "[Test 15.3] Reserved task can be claimed with token after review attaches"
resp=$(tool_call 19 "claimIssueTask" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK2_ID\",\"next_step_token\":\"$NEXT_STEP_TOKEN\"}")
assert_contains "reserved claim ok" "$resp" "in_progress"

# Wait for submit to finish (unblocked by review)
SUBMIT_RESP=$(wait_resp 17)
assert_contains "submit unblocked" "$SUBMIT_RESP" "done"
echo ""

# --- Test 15.4: Close Issue requires all tasks done ---
echo "[Test 15.4] Close Issue requires all tasks done"
resp=$(tool_call 23 "closeIssue" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"summary\":\"try close before all done\"}")
assert_contains "close rejected" "$resp" "cannot close issue"
echo ""

# --- Test 15.5: Submit + Review Task-2, then close issue ---
echo "[Test 15.5] Submit + Review Task-2, then close issue"
tool_call_async 24 "submitIssueTask" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK2_ID\",\"artifacts\":{\"summary\":\"Implemented task-2\",\"changed_files\":[\"auth/service.go\"],\"diff\":\"diff --git a/auth/service.go b/auth/service.go\n\",\"links\":[\"https://example.com/diff2\"],\"test_cases\":[\"go test ./...\"],\"test_result\":\"PASS\",\"test_output\":\"ok\tmodule\t0.01s\"}}"

# Wait for submitted signal
resp=""
for _ in $(seq 1 10); do
    resp=$(tool_call 25 "waitIssueTaskEvents" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\"}")
    if echo "$resp" | grep -Fq "issue_task_submitted"; then
        if echo "$resp" | grep -Fq "$TASK2_ID"; then
            break
        fi
    fi
done
assert_contains "returns submitted signal for task-2" "$resp" "$TASK2_ID"

# Derive worker_id from task-2 submitter.
resp=$(tool_call 26 "getIssueTask" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK2_ID\"}")
WORKER2_ID=$(echo "$resp" | grep -oE 'm_[0-9]+_[0-9a-f]+' | head -1)
assert_contains "task-2 has submitter" "$resp" "$WORKER2_ID"

resp=$(tool_call 27 "getNextStepToken" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK2_ID\",\"worker_id\":\"$WORKER2_ID\",\"completion_score\":5}")
NEXT2_TOKEN=$(echo "$resp" | grep -oE 'ns_[0-9]+_[0-9a-f]+' | head -1)
assert_contains "mints next_step_token2" "$resp" "$NEXT2_TOKEN"

resp=$(tool_call 28 "reviewIssueTask" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK2_ID\",\"verdict\":\"approved\",\"feedback\":\"LGTM\",\"completion_score\":5,\"artifacts\":{\"review_summary\":\"Reviewed diff + tests\",\"reviewed_refs\":[\"auth/service.go\",\"https://example.com/diff2\"]},\"feedback_details\":[{\"dimension\":\"correctness\",\"severity\":\"info\",\"content\":\"OK\"}],\"next_step_token\":\"$NEXT2_TOKEN\"}")
assert_contains "task-2 status done" "$resp" "done"

# Wait for submit to finish (unblocked by review)
SUBMIT2_RESP=$(wait_resp 24)
assert_contains "submit2 unblocked" "$SUBMIT2_RESP" "done"

resp=$(tool_call 29 "closeIssue" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"summary\":\"all tasks done\"}")
assert_contains "issue closed" "$resp" "\\\"status\\\": \\\"done\\\""
echo ""

# --- Test 16: Lock Files ---
echo "[Test 16] Lock Files"
resp=$(tool_call 20 "lockFiles" "$WORKER_SESSION" "{\"task_id\":\"$TASK_ID\",\"files\":[\"auth/handler.go\"],\"ttl_sec\":60}")
assert_contains "lock acquired" "$resp" "lease_id"
LEASE_ID=$(echo "$resp" | grep -oE 'l_[0-9]+_[0-9a-f]+' | head -1)
echo "  (lease_id=$LEASE_ID)"
echo ""

# --- Test 17: Heartbeat ---
echo "[Test 17] Heartbeat"
resp=$(tool_call 21 "heartbeat" "$WORKER_SESSION" "{\"lease_id\":\"$LEASE_ID\",\"extend_sec\":60}")
assert_contains "extends lease" "$resp" "expires_at"
echo ""

# --- Test 18: Unlock ---
echo "[Test 18] Unlock"
resp=$(tool_call 22 "unlock" "$WORKER_SESSION" "{\"lease_id\":\"$LEASE_ID\"}")
assert_not_contains "no error" "$resp" "isError"
echo ""

# --- Test 19: Trace audit ---
echo "[Test 19] Trace audit log exists"
trace_file="$SWARM_MCP_ROOT/trace/events.jsonl"
if [ -f "$trace_file" ]; then
    assert_contains "Trace file exists" "Trace file exists with $(wc -l < "$trace_file") events" "events"
else
    assert_contains "Trace file missing" "Trace file missing" "exists"
fi

echo ""

# --- Test 20: Issue lease expiry auto-cancels issue ---
echo "[Test 20] Issue lease expiry auto-cancels issue"
resp=$(tool_call 40 "createIssue" "$LEAD_SESSION" "{\"subject\":\"expiry issue\",\"description\":\"expiry\",\"user_issue_doc\":{\"name\":\"user\",\"content\":\"u\"},\"lead_issue_doc\":{\"name\":\"lead\",\"content\":\"l\"}}")
EXP_ISSUE_ID=$(echo "$resp" | grep -oE 'issue_[0-9]+_[0-9a-f]+' | head -1)
assert_contains "expiry issue created" "$resp" "$EXP_ISSUE_ID"
assert_contains "issue has lease_expires_at" "$resp" "lease_expires_at"
assert_contains "issue has server_now" "$resp" "server_now"

resp=$(tool_call 41 "extendIssueLease" "$LEAD_SESSION" "{\"issue_id\":\"$EXP_ISSUE_ID\",\"extend_sec\":1}")
assert_contains "issue lease extended" "$resp" "lease_expires_at_ms"

sleep 2
resp=$(tool_call 42 "getIssue" "$LEAD_SESSION" "{\"issue_id\":\"$EXP_ISSUE_ID\"}")
assert_contains "issue auto canceled" "$resp" "\\\"status\\\": \\\"canceled\\\""
echo ""

# --- Test 21: Task lease expiry auto-reopens task ---
echo "[Test 21] Task lease expiry auto-reopens task"
resp=$(tool_call 43 "createIssue" "$LEAD_SESSION" "{\"subject\":\"expiry task issue\",\"description\":\"expiry\",\"user_issue_doc\":{\"name\":\"user\",\"content\":\"u\"},\"lead_issue_doc\":{\"name\":\"lead\",\"content\":\"l\"}}")
EXP2_ISSUE_ID=$(echo "$resp" | grep -oE 'issue_[0-9]+_[0-9a-f]+' | head -1)
assert_contains "expiry task issue created" "$resp" "$EXP2_ISSUE_ID"
assert_contains "issue has lease_expires_at" "$resp" "lease_expires_at"
assert_contains "issue has server_now" "$resp" "server_now"

resp=$(tool_call 44 "createIssueTask" "$LEAD_SESSION" "{\"issue_id\":\"$EXP2_ISSUE_ID\",\"subject\":\"exp task\",\"description\":\"d\",\"difficulty\":\"easy\",\"points\":1,\"spec\":{\"name\":\"spec\",\"goal\":\"g\",\"rules\":\"r\",\"constraints\":\"c\",\"conventions\":\"k\",\"acceptance\":\"a\",\"impact_scope\":\"i\",\"split_from\":\"s\",\"split_reason\":\"sr\"}}")
EXP_TASK_ID=$(echo "$resp" | grep -oE 'task-[0-9]+' | head -1)
assert_contains "expiry task created" "$resp" "$EXP_TASK_ID"
assert_contains "task has lease_expires_at" "$resp" "lease_expires_at"
assert_contains "task has server_now" "$resp" "server_now"

resp=$(tool_call 45 "claimIssueTask" "$WORKER_SESSION" "{\"issue_id\":\"$EXP2_ISSUE_ID\",\"task_id\":\"$EXP_TASK_ID\"}")
assert_contains "expiry task claimed" "$resp" "in_progress"

resp=$(tool_call 46 "extendIssueTaskLease" "$WORKER_SESSION" "{\"issue_id\":\"$EXP2_ISSUE_ID\",\"task_id\":\"$EXP_TASK_ID\",\"extend_sec\":1}")
assert_contains "task lease extended" "$resp" "lease_expires_at_ms"

sleep 2
resp=$(tool_call 47 "getIssueTask" "$LEAD_SESSION" "{\"issue_id\":\"$EXP2_ISSUE_ID\",\"task_id\":\"$EXP_TASK_ID\"}")
assert_contains "task reopened" "$resp" "\\\"status\\\": \\\"open\\\""

echo ""

echo "==============================="
echo "Results: $PASS passed, $FAIL failed"
echo "==============================="

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
