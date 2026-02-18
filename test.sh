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

	# Stop gateway first (if started)
	if [ -n "${GW_PID:-}" ]; then
		kill "$GW_PID" >/dev/null 2>&1 || true
	fi

    if [ -n "${RESP_READER_PID:-}" ]; then
        kill "$RESP_READER_PID" >/dev/null 2>&1 || true
    fi
    if [ -n "${SWARM_MCP_PROC_PID:-}" ]; then
        kill "$SWARM_MCP_PROC_PID" >/dev/null 2>&1 || true
    fi
    trash "$SWARM_MCP_ROOT" >/dev/null 2>&1 || true
}
trap cleanup EXIT

GW_TOKEN="a-very-long-random-token"
GW_HOST="127.0.0.1"
GW_PORT="15411"
GW_URL="http://$GW_HOST:$GW_PORT"

echo "Starting mcp-gateway on $GW_URL"

# Basic port check to avoid conflicts.
if lsof -nP -iTCP:"$GW_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "ERROR: port already in use: $GW_PORT"
    exit 1
fi

MCP_ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
GW_DIR="$MCP_ROOT_DIR/mcp-gateway"
SESSION_DIR="$MCP_ROOT_DIR/session-mcp"

GW_BIN="$GW_DIR/bin/mcp-gateway"
if [ ! -x "$GW_BIN" ]; then
    mkdir -p "$GW_DIR/bin"
    (cd "$GW_DIR" && go build -o "$GW_BIN" .)
fi

# Build session-mcp binary so gateway local route can start it.
SESSION_BIN="$SESSION_DIR/bin/session-mcp"
if [ ! -x "$SESSION_BIN" ]; then
    mkdir -p "$SESSION_DIR/bin"
    (cd "$SESSION_DIR" && go build -o "$SESSION_BIN" .)
fi

# Start gateway (routes.yaml is resolved relative to rootDir; use default root=~/Coding/mcp by leaving -root empty).
MCP_GATEWAY_TOKEN="$GW_TOKEN" "$GW_BIN" -listen "$GW_HOST:$GW_PORT" -routes "$GW_DIR/routes.yaml" >/dev/null 2>&1 &
GW_PID=$!

# Wait for gateway health
for _ in $(seq 1 200); do
    if curl -s "$GW_URL/healthz" | grep -q ok; then
        break
    fi
    sleep 0.05
done

session_mcp_tool_call() {
    local tool="$1"
    local args_json="$2"
    curl -s -X POST "$GW_URL/mcps/session-mcp" \
      -H 'Content-Type: application/json' \
      -H "Authorization: Bearer $GW_TOKEN" \
      -d "{\"jsonrpc\":\"2.0\",\"method\":\"tools/call\",\"params\":{\"name\":\"$tool\",\"arguments\":$args_json},\"id\":1}"
}

echo "Opening semantic sessions via session-mcp.upsertSemanticSession"
LEAD_UP=$(session_mcp_tool_call "upsertSemanticSession" '{"project_identity":{"repo_name":"mcp","branch_name":"test"},"business_context":{"issue_key":"NO-TICKET","feature_module":"SwarmLead","user_story_summary":"Lead window"},"workflow_state":{"intent_phase":"IMPLEMENTATION","key_modified_files":["swarm-mcp/internal/mcp/server.go"],"primary_error_signature":"N/A"}}')
LEAD_SESSION=$(echo "$LEAD_UP" | grep -oE 'ss_[0-9]+' | head -1)

sleep 0.01
WORKER_UP=$(session_mcp_tool_call "upsertSemanticSession" '{"project_identity":{"repo_name":"mcp","branch_name":"test"},"business_context":{"issue_key":"NO-TICKET","feature_module":"SwarmWorker","user_story_summary":"Worker window"},"workflow_state":{"intent_phase":"IMPLEMENTATION","key_modified_files":["swarm-mcp/internal/mcp/tools.go"],"primary_error_signature":"N/A"}}')
WORKER_SESSION=$(echo "$WORKER_UP" | grep -oE 'ss_[0-9]+' | head -1)

sleep 0.01
ACCEPTOR_UP=$(session_mcp_tool_call "upsertSemanticSession" '{"project_identity":{"repo_name":"mcp","branch_name":"test"},"business_context":{"issue_key":"NO-TICKET","feature_module":"SwarmAcceptor","user_story_summary":"Acceptor window"},"workflow_state":{"intent_phase":"IMPLEMENTATION","key_modified_files":["swarm-mcp/internal/swarm/delivery.go"],"primary_error_signature":"N/A"}}')
ACCEPTOR_SESSION=$(echo "$ACCEPTOR_UP" | grep -oE 'ss_[0-9]+' | head -1)

if [ -z "$LEAD_SESSION" ] || [ -z "$WORKER_SESSION" ] || [ -z "$ACCEPTOR_SESSION" ]; then
    echo "FAILED to obtain session ids"
    echo "lead_up=$LEAD_UP"
    echo "worker_up=$WORKER_UP"
    echo "acceptor_up=$ACCEPTOR_UP"
    exit 1
fi
echo "  (lead_session=$LEAD_SESSION)"
echo "  (worker_session=$WORKER_SESSION)"
echo "  (acceptor_session=$ACCEPTOR_SESSION)"

# Ensure swarm-mcp validation uses this gateway (must be exported BEFORE starting swarm-mcp).
export SESSION_MCP_GATEWAY_URL="$GW_URL"
export MCP_GATEWAY_TOKEN="$GW_TOKEN"

# Persistent stdio server process.
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
    for _ in $(seq 1 4000); do
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
    for _ in $(seq 1 4000); do
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

echo "[Test 1] Session ID"
echo "(gateway ready; semantic sessions acquired)"
echo ""

# --- Test 1.1: Tools List ---
echo "[Test 1.1] Tools List"
resp=$(call 3 "tools/list" '{}')
assert_contains "lists lockFiles tool" "$resp" "lockFiles"
assert_contains "lists createIssue tool" "$resp" "createIssue"
assert_contains "lists myProfile" "$resp" "myProfile"
assert_not_contains "does not expose openSession" "$resp" "openSession"
echo ""

# --- Test 1.2: myProfile ---
echo "[Test 1.2] myProfile"
resp=$(tool_call 4 "myProfile" "$LEAD_SESSION" '{}')
assert_contains "myProfile returns member_id" "$resp" "member_id"
echo ""

# --- Test 2: Create Issue + Task ---
echo "[Test 2] Create Issue"
resp=$(tool_call 5 "createIssue" "$LEAD_SESSION" '{"subject":"Test Issue","description":"For integration tests","user_issue_doc":{"name":"user_issue","content":"User provided context"},"lead_issue_doc":{"name":"lead_issue","content":"Lead refined context"}}')
assert_contains "creates issue" "$resp" "issue_"
ISSUE_ID=$(echo "$resp" | grep -oE 'issue_[0-9]+_[0-9a-f]+' | head -1)
if [ -z "$ISSUE_ID" ]; then
    echo "FAILED to parse ISSUE_ID: $resp"
    exit 1
fi

echo "[Test 2.1] Create Issue Task"
resp=$(tool_call 6 "createIssueTask" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"subject\":\"Task 1\",\"difficulty\":\"easy\",\"spec\":{\"name\":\"spec\",\"goal\":\"g\",\"rules\":\"r\",\"constraints\":\"c\",\"conventions\":\"k\",\"acceptance\":\"a\",\"impact_scope\":\"i\",\"split_from\":\"s\",\"split_reason\":\"sr\"}}")
assert_contains "creates task" "$resp" "task-"
TASK_ID=$(echo "$resp" | grep -oE 'task-[0-9]+' | head -1)
if [ -z "$TASK_ID" ]; then
    echo "FAILED to parse TASK_ID: $resp"
    exit 1
fi
echo ""

# --- Test 3: Worker claims and submits, lead reviews ---
echo "[Test 3] Claim Issue Task"
resp=$(tool_call 7 "claimIssueTask" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\"}")
assert_contains "claimed in_progress" "$resp" "in_progress"

echo "[Test 3.1] Submit Issue Task (async)"
tool_call_async 8 "submitIssueTask" "$WORKER_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\",\"artifacts\":{\"summary\":\"done\",\"changed_files\":[\"a.txt\"],\"test_cases\":[\"go test ./...\"],\"test_output\":\"ok\",\"test_result\":\"passed\"}}"

echo "[Test 3.2] Lead waitIssueTaskEvents until submitted"
resp=""
for _ in $(seq 1 30); do
    resp=$(tool_call 9 "waitIssueTaskEvents" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"timeout_sec\":5}")
    if echo "$resp" | grep -Fq "issue_task_submitted"; then
        break
    fi
done
assert_contains "sees submitted" "$resp" "issue_task_submitted"

echo "[Test 3.3] Review Issue Task"
resp=$(tool_call 10 "getIssueTask" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\"}")
WORKER_ID=$(echo "$resp" | grep -oE '"submitter"\s*:\s*"m_[0-9]+_[0-9a-f]+"' | grep -oE 'm_[0-9]+_[0-9a-f]+' | head -1)
if [ -z "$WORKER_ID" ]; then
    # fallback: first member-like token in response
    WORKER_ID=$(echo "$resp" | grep -oE 'm_[0-9]+_[0-9a-f]+' | head -1)
fi
if [ -z "$WORKER_ID" ]; then
    echo "FAILED to parse WORKER_ID from getIssueTask: $resp"
    exit 1
fi

resp=$(tool_call 11 "getNextStepToken" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\",\"worker_id\":\"$WORKER_ID\",\"completion_score\":5}")
NEXT_STEP_TOKEN=$(echo "$resp" | grep -oE 'ns_[0-9]+_[0-9a-f]+' | head -1)
if [ -z "$NEXT_STEP_TOKEN" ]; then
    echo "FAILED to parse next_step_token from getNextStepToken: $resp"
    exit 1
fi

resp=$(tool_call 12 "reviewIssueTask" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"task_id\":\"$TASK_ID\",\"verdict\":\"approved\",\"completion_score\":5,\"artifacts\":{\"review_summary\":\"ok\",\"reviewed_refs\":[\"a.txt\"]},\"feedback_details\":[{\"dimension\":\"correctness\",\"severity\":\"info\",\"content\":\"ok\"}],\"next_step_token\":\"$NEXT_STEP_TOKEN\"}")
assert_contains "task done" "$resp" "done"

SUBMIT_RESP=$(wait_resp 8)
assert_contains "submit unblocked" "$SUBMIT_RESP" "done"
echo ""

# --- Test 4: Delivery acceptance ---
echo "[Test 4] submitDelivery (async)"
tool_call_async 20 "submitDelivery" "$LEAD_SESSION" "{\"issue_id\":\"$ISSUE_ID\",\"summary\":\"deliver\",\"artifacts\":{\"changed_files\":[\"a.txt\"],\"reviewed_refs\":[\"a.txt\"],\"test_cases\":[\"go test ./...\"],\"test_result\":\"passed\"},\"timeout_sec\":20}"

echo "[Test 4.1] Acceptor waits + reviews"
resp=""
for _ in $(seq 1 30); do
    resp=$(tool_call 21 "waitDeliveries" "$ACCEPTOR_SESSION" '{"status":"open","timeout_sec":5}')
    if echo "$resp" | grep -Fq "delivery_"; then
        break
    fi
done
assert_contains "waitDeliveries returns" "$resp" "delivery_"
DELIVERY_ID=$(echo "$resp" | grep -oE 'delivery_[0-9]+_[0-9a-f]+' | head -1)
if [ -z "$DELIVERY_ID" ]; then
    echo "FAILED to parse DELIVERY_ID: $resp"
    exit 1
fi
resp=$(tool_call 22 "claimDelivery" "$ACCEPTOR_SESSION" "{\"delivery_id\":\"$DELIVERY_ID\"}")
assert_contains "claimed" "$resp" "in_review"
resp=$(tool_call 23 "reviewDelivery" "$ACCEPTOR_SESSION" "{\"delivery_id\":\"$DELIVERY_ID\",\"verdict\":\"approved\",\"completion_score\":5,\"feedback_details\":[{\"dimension\":\"correctness\",\"severity\":\"info\",\"content\":\"ok\"}]}" )
assert_contains "approved" "$resp" "approved"

DELIVER_RESP=$(wait_resp 20)
assert_contains "submitDelivery unblocked" "$DELIVER_RESP" "reviewed"
echo ""
