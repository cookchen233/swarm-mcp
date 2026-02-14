# Swarm MCP (Issue-Centric)

一个用于多窗口多智能体协作的本地 MCP Server（stdio 模式），可接入任意兼容 MCP 的客户端（MCP host）。

[English](README.en.md) | **中文**

本项目采用 **issue-centric** 协作模型：

- **Issue**：一个主问题（工作池容器）
- **Task**：可被任意 worker 领取的工作单元（不分配到固定人）
- 通过 **事件流**（long-poll）进行 lead/worker 协作
- 通过 **文件锁（租约）** 防止并发改文件冲突
- 通过 **文档库（docs library）** 在 lead/worker 之间传递上下文

## 特性

- Issue 池：任务以池的形式散播，worker 自由认领
- Worker 身份登记：worker 领取任务后可追踪“谁做了什么”
- 事件流：
  - `waitIssueTaskEvents`：issue 级 select-like
- 阻塞式问答：`askIssueTask`（worker 提问后阻塞等待 lead reply）
- Task 状态机联动：
  - `kind=question|blocker` → task 自动置为 `blocked`
  - `kind=reply` → task 自动解除回 `in_progress`
- 严格模式（默认）：在 `tools/list` 中隐藏非阻塞提问原语，引导 worker 走阻塞式 ask

## 设计要点

### 核心决策

- **服务形态**：本地 MCP Server（stdio），由 MCP host 拉起；所有窗口共享同一个数据目录（`SWARM_MCP_ROOT`）
- **协作主通道**：Issue/Task 事件流（lead 使用 `waitIssueTaskEvents` 做 select-like 循环）
- **并发控制**：租约制文件锁（`lockFiles` + `heartbeat` + `unlock`）
- **持久化**：文件系统（默认 `~/.swarm-mcp/`）

### Task 状态机

```text
open -> in_progress -> submitted -> done
                   -> blocked
                   -> canceled
```

与消息联动：

- `askIssueTask(kind=question|blocker)` 或 `postIssueTaskMessage(kind=question|blocker)` 会把 task 自动置为 `blocked`
- lead 通过 `replyIssueTaskMessage` 回复后，task 自动解除回 `in_progress`

### 文件锁语义（必须理解）

- **租约制**：默认 TTL 120s；建议每 30s `heartbeat` 续租
- **多文件原子锁**：`lockFiles(files=[...])` 要么全成功，要么全失败
- **跨进程安全**：写入操作使用全局文件锁（`$SWARM_MCP_ROOT/.global.lock`）保证多窗口一致性
- **过期抢占**：租约过期后可被其他人获取，审计日志会记录

### Strict 模式说明

- 严格模式默认开启：`SWARM_MCP_STRICT != "0"`
- 严格模式下：`tools/list` **隐藏** `postIssueTaskMessage`，引导 worker 使用阻塞式 `askIssueTask`
- 说明：当前实现是“隐藏工具”，不是“硬拒绝调用”。（如需要可再加服务端拒绝策略）

### ID / 会话机制（为什么要显式传 issue_id/task_id）

- MCP 工具调用默认是“显式参数”风格：本 server 不会为你维护“当前 issue/task 上下文”
- 你忘了 ID 时的恢复方式：
  - `listIssues` / `getIssue`
  - `listIssueTasks(issue_id)` / `getIssueTask`

另外：本 server 引入了 **session_id（强约束）** 作为“窗口隔离令牌”（类似 cookie 的作用）：

- 每个窗口必须先调用 `openSession` 拿到自己的 `session_id`
- 后续 **所有 tools/call 都必须携带 `session_id`**，否则服务端会直接报错

### Docs Library：shared / issue / task 的区别

- **Shared docs（全局）**：所有 issue 通用的文档
  - 落盘：`$SWARM_MCP_ROOT/docs/shared/<name>.md`
  - 工具：`writeSharedDoc` / `readSharedDoc` / `listSharedDocs`
- **Issue docs（单个 issue）**：该 issue 专属的规格/决策/上下文
  - 落盘：`$SWARM_MCP_ROOT/issues/<issue_id>/docs/<name>.md`
  - 工具：`writeIssueDoc` / `readIssueDoc` / `listIssueDocs`
- **Task docs（单个 task）**：该 task 的补充材料/交付物（可选）
  - 落盘：`$SWARM_MCP_ROOT/issues/<issue_id>/tasks/<task_id>/docs/<name>.md`
  - 工具：`writeTaskDoc` / `readTaskDoc` / `listTaskDocs`

建议：

- 如果你不想维护全局 shared，可以只用 issue docs
- 如果你希望把协作规范沉淀成“团队标准”，shared docs 更合适

### 两阶段注入（推荐使用姿势）

```text
Phase 1（自由分析期）
- 不提 MCP，不调用工具
- 先完成目标澄清、风险识别、拆解与验证计划

Phase 2（协作注入期）
- 明确进入协作：createIssue / createIssueTask
- 之后所有修改都必须先 lockFiles，再编码；持锁 heartbeat；完成 unlock
```

#### Phase 2：提示词与最小流程（可直接复制）

下面给出一套可直接复制到 MCP host 的 **lead / worker 提示词模板**，以及一条最小可跑通的协作流程。

##### Lead 提示词

```text
现在请你将开发清单写入一个文档， 用于后续逐一验收

然后进入协作阶段：我这里有一个叫 swarm-mcp 的协作 MCP Server
你需要：
1) 基于你刚才的分析，重新规划一次（允许重拆分/重排序）以适配多人协作
2) 然后用 swarm-mcp 自主完成：创建 issue、拆分 task，并把 issue 散播出去让各 worker 自行领取 task

[角色]
你是 Lead。

[协作规则]
- 必须先调用 openSession 拿到 session_id，后续所有工具调用都必须携带该 session_id
- 先查看是否有与本 issue 的主题几乎相同并尚未关闭的 issue, 如果有, 请直接先关闭它重建
- 推荐流程：createIssue -> createIssueTask -> waitIssueTaskEvents -> review/reply
- 未经明确要求：你必须只做 lead（拆分/答疑/验收/事件循环），不要自己下场实现需求、不要去改 worker 的目标代码文件
- Q&A：worker 用 askIssueTask；你必须用同一个 issue_id/task_id 通过 replyIssueTaskMessage 回复
- issue 有超时关闭机制，根据 createIssue/getIssue 返回的过期时间，请在过期前5分钟使用 extendIssueLease 及时续约(可通过 swarmNow 获取当前时间)
- **强约束** 调用 waitIssueTaskEvents 接收到事件后需要仔细推理分析并处理该事件
- **强约束** 处理完事件后需要继续调用 waitIssueTaskEvents 直到所有 tasks 被完成并调用 closeIssue
```

##### Worker 提示词

```text
你当前处于 MCP 协作模式，可以调用 swarm-mcp 提供的工具来完成任务
如果你在 MCP host 里看不到 swarm-mcp 工具：先尝试 tools/list

[角色]
你是 Worker。

[协作规则]
- 必须先调用 openSession 拿到 session_id，后续所有工具调用都必须携带该 session_id
- 领取任务：listIssueTasks -> 找到 open -> claimIssueTask
- 当你发现没有任何 open 状态 的 issues 或 tasks 时，请务必直接调用 waitIssues 或 waitIssueTasks
- 开工前先补齐上下文：优先阅读 task 的 doc_paths / required_*_docs 指向的文档（用 readIssueDoc / readTaskDoc）
- 修改代码前必须加锁：lockFiles(files=[...])，没有有效 lockFiles 锁，不要修改任何文件，持锁期间每 ~30s 续租：heartbeat，每完成一个文件的修改后必须释放该文件锁：unlock
- 若开发中因信息不足而不确定/遇到阻塞：使用 askIssueTask(kind=question|blocker) 获取 lead 的决策，然后继续推进
- task 有超时关闭机制，根据 claimIssueTask/getIssueTask 返回的过期时间，请在过期前5分钟使用 extendIssueTaskLease 及时续约(可通过 swarmNow 获取当前时间)
- **强约束** 完成任务后提交：submitIssueTask，根据返回的 next_actions 继续进行下一步
- **强约束** 当所有 tasks 被完成后继续调用 waitIssues 或 waitIssueTasks
```

##### 最小流程示例（端到端）

###### 阻塞/挂起语义速查

说明：本 server 的若干工具会“挂起”（阻塞）以配合被动事件循环与严格协作。

| 工具 | 是否挂起 | 何时返回 | 默认/固定超时 | 备注 |
| --- | --- | --- | --- | --- |
| `waitIssueTaskEvents(issue_id)` | 是 | 仅当出现 `question/blocker` 或 `issue_task_submitted` 信号 | 固定 600s | Lead 被动事件循环；一次最多返回 1 条 signal；忽略其它事件并继续挂起 |
| `submitIssueTask(issue_id, task_id, ...)` | 是 | 提交后，直到 lead `reviewIssueTask` 产生 `reviewed/resolved` 事件 | 固定 600s | 提交必须携带结构化 `artifacts`；用于防止 worker 提交后立即结束对话 |
| `askIssueTask(issue_id, task_id, ...)` | 是 | lead `replyIssueTaskMessage` 回复后 | 默认 600s（可传） | 会先发出 `question/blocker` 再等待 reply |
| `lockFiles(...)` | 可能 | 拿到锁即返回；若被占用则等待到 `wait_sec` | `wait_sec` | 不会无限挂起，超时会失败返回 |

另外：同一 issue 内的 `task_id` 为递增序列：`task-1`、`task-2`…（不会与其它 issue 冲突）。

1. **Lead 创建工作池**
   - `createIssue(subject="...", description="...", user_issue_doc={name:"user", content:"..."}, lead_issue_doc={name:"lead", content:"..."}, user_other_docs=[{name:"context", content:"..."}])`
   - `createIssueTask(issue_id, subject="...", description="...", difficulty="easy|medium|focus", context_task_ids=[...], suggested_files=[...], spec={name:"spec", split_from:"...", split_reason:"...", impact_scope:"...", context_task_ids:[...], goal:"...", rules:"...", constraints:"...", conventions:"...", acceptance:"..."})`

2. **Worker 领取并实现**
   - （可选）当 lead 尚未创建 issue 时，你可以先调用 `waitIssues(after_count=0, timeout_sec=600)` 阻塞等待 issue 出现
   - （可选）当你已知道 `issue_id` 但 lead 尚未创建 tasks 时，你可以调用 `waitIssueTasks(issue_id, after_count=0, timeout_sec=600)` 阻塞等待 task 出现
   - `listIssueTasks(issue_id)` -> 找 `open`
   - `claimIssueTask(issue_id, task_id)`（若该 task 被 lead 预留，则必须带 `next_step_token`）
   - `lockFiles(task_id, files=["path/to/file.go"], ttl_sec=120, wait_sec=60)`
   - （编码；期间 `heartbeat(lease_id)`）
   - `unlock(lease_id)`
   - `submitIssueTask(issue_id, task_id, artifacts={summary:"...", changed_files:[...], diff:"...", links:[...]})` -> 挂起等待 lead review

   Worker 侧建议把整个过程当作一个循环执行：

   - `submitIssueTask` 返回后不要结束对话；它返回的结果里会包含 `next_actions`，你应继续执行其中的动作（例如 `listIssueTasks` / `claimIssueTask`）来领取下一项工作。
   - 重复「领取 -> 锁文件 -> 实现 -> 提交」直到：
     - `listIssueTasks(issue_id, status="open")` 为空（没有可领取任务），或
     - lead 明确结束该 issue / 不再派发任务。

3. **Lead 验收或打回**
   - `waitIssueTaskEvents(issue_id)` -> 收到 submitted/question/blocker
   - `getNextStepToken(issue_id, task_id, worker_id, completion_score=1|2|5)` -> 服务端自动挑选并预留 next task，返回 `next_step_token`
   - `reviewIssueTask(issue_id, task_id, verdict="approved|rejected", feedback="...", completion_score=1|2|5, artifacts={review_summary:"...", reviewed_refs:[...]}, feedback_details=[...], next_step_token="...")`

4. **Q&A（严格模式推荐）**
   - Worker：`askIssueTask(issue_id, task_id, kind="question", content="...", timeout_sec=600)`
   - Lead：`replyIssueTaskMessage(issue_id, task_id, content="...")`

## 安装与运行

### 编译

```bash
go build -o bin/swarm-mcp ./cmd/swarm-mcp/
```

### 启动（stdio）

该 server 以 MCP stdio 方式运行，由 MCP host 启动与管理。

## MCP Client 配置

请在你的 MCP host（客户端）的 MCP 配置中添加本 server。不同客户端的配置文件路径与格式可能不同。

添加 server：

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

### 环境变量

- `SWARM_MCP_ROOT`
  - 数据目录根路径
  - **默认值**：`$HOME/.swarm-mcp`
  - 建议按项目隔离：`~/.swarm-mcp/<project_key>`
- `SWARM_MCP_STRICT`
  - 工具暴露“严格模式”开关
  - **默认开启严格模式**：只要不是 `0` 都视为开启
  - 设置为 `0`：在 `tools/list` 中返回全量工具（包含 `postIssueTaskMessage`）

## 数据目录结构

默认根目录：`~/.swarm-mcp/`（或 `SWARM_MCP_ROOT`）

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

## 协作工作流（推荐）

### Lead（主窗口）

1. `createIssue`
2. `createIssueTask`（拆成多个可并行的 tasks）
3. `waitIssueTaskEvents`（select-like 阻塞等待提交/问题/阻塞）
4. `replyIssueTaskMessage` / `reviewIssueTask`

### Worker（执行窗口）

1. `listIssueTasks` → 找到 `open` 的 task
2. `claimIssueTask`（会记录当前窗口 `member_id`）
3. `lockFiles`（修改文件前必须加锁）
4. 编码（持锁期间 `heartbeat`）
5. `unlock`
6. `submitIssueTask`

### Worker 提问（严格模式推荐路径）

- 使用 `askIssueTask(kind=question|blocker, ...)`
  - 该调用会阻塞直到 lead 使用 `replyIssueTaskMessage` 回复

## 手动验证（推荐跑一遍）

> 下面以“两个 MCP client 窗口（lead/worker）”为例做手动验证；如果你的 MCP host 支持多会话/多窗口，可直接照做。

### Step 0：配置 MCP Server

参考上面的 “MCP Client 配置”。建议设置：

- `SWARM_MCP_ROOT=~/.swarm-mcp/<project_key>`（按项目隔离）
- `SWARM_MCP_STRICT=1`（默认严格模式）
- `SWARM_MCP_SUGGESTED_MIN_TASK_COUNT=2`（建议最少任务数）
- `SWARM_MCP_MAX_TASK_COUNT=10`（每个 issue 最大任务数；在 createIssueTask 时强制）
- `SWARM_MCP_ISSUE_TTL_SEC=3600`（issue 续约超时时间；过期自动标记为 canceled）
- `SWARM_MCP_TASK_TTL_SEC=600`（task 续约超时时间；过期会把 in_progress/blocked/submitted 自动回到 open 以便继续）

重启 MCP host 后，每个窗口/会话都能看到 swarm-mcp 工具。

### Step 1：开两个窗口

- **窗口 A（Lead）**：只做拆解、答疑、验收（通常不直接改代码）
- **窗口 B（Worker）**：领取任务、锁文件、编码、提交

每个窗口都有自己的 `member_id`（用 `whoAmI` 查看），用于审计与追踪。

### Step 2：Lead 创建 issue 与 tasks

- `createIssue`
- （可选）`extendIssueLease`（issue 续约，避免长时间无人操作被自动取消）
- 多次 `createIssueTask`（必须设置 `difficulty=easy|medium|focus`）
- 开始 `waitIssueTaskEvents` 循环等待事件

### Step 3：Worker 领取任务并修改

- `listIssueTasks` -> 找 `open`
- `claimIssueTask`
- （可选）`extendIssueTaskLease`（task 续约，避免窗口关闭/长时间无动作导致任务自动回到 open）
- `lockFiles`（锁定本 task 相关文件）
- 编码（期间 `heartbeat`）
- `unlock`
- `submitIssueTask`

### Step 4：Lead 验收

- `waitIssueTaskEvents` 收到 submitted
- `getNextStepToken` 生成下一步 token（服务端自动派发并预留，`end` 或 `claim_task`）
- `reviewIssueTask`（必须携带 `completion_score`、`feedback_details`、`artifacts` 与 `next_step_token`）

### Step 5：提问/阻塞（验证 strict + ask）

- worker：`askIssueTask(kind=question|blocker, ...)`（会阻塞）
- lead：`replyIssueTaskMessage(...)`
- 观察 task 状态：`in_progress -> blocked -> in_progress`

## 关键场景验证（避免线上踩坑）

### 锁冲突

1. worker 先 `lockFiles` 某文件
2. lead 或另一个 worker 立刻锁同一文件（`wait_sec=0`）应失败

### 锁过期抢占

1. worker `lockFiles`，设置较小 `ttl_sec`，并不做 heartbeat
2. 等过期后其他窗口再 `lockFiles` 同一文件应成功
3. 检查 `trace/events.jsonl` 有过期相关审计记录

### 多文件原子锁

1. `lockFiles(files=["a.go","b.go"])`
2. 另一个窗口锁 `b.go` 相关集合应失败
3. 释放后重试应成功

## 审计日志

所有操作记录在：

- `$SWARM_MCP_ROOT/trace/events.jsonl`

建议排查方式：

```bash
grep lock "$SWARM_MCP_ROOT/trace/events.jsonl"
```

## Issue/Task 自动过期回收（韧性机制）

本项目不依赖后台定时器；过期回收在常用 tool 调用入口同步触发。

### 触发时机（何时执行回收）

只要有窗口在调用下列 tools，就会顺带执行一次过期回收（best-effort）：

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

### 回收规则（做了什么）

- issue 过期：`open|in_progress` -> `canceled`，并追加事件 `issue_expired`
- task 过期：`in_progress|blocked|submitted` -> `open`（可重新领取），并追加事件 `issue_task_expired`

### 返回字段（如何知道何时续约）

所有返回 issue/task 的工具（例如 `createIssue/getIssue/listIssues`、`createIssueTask/getIssueTask/listIssueTasks`、`claimIssueTask`、`extendIssueLease/extendIssueTaskLease`）都会附带：

- `lease_expires_at_ms`：过期时间（Unix 毫秒）
- `lease_expires_at`：过期时间（RFC3339，自然时间，UTC）
- `server_now_ms`：服务端当前时间（Unix 毫秒）
- `server_now`：服务端当前时间（RFC3339，UTC）

### 时间对表（避免客户端时钟偏差）

- 使用 `swarmNow` 获取服务端当前时间：返回 `now_ms` + `now`（RFC3339，UTC）

## 清理数据（重新开始）

```bash
trash "$SWARM_MCP_ROOT"
```

## 排障（Troubleshooting）

### 1) MCP host 里看不到 swarm-mcp 工具

- **检查 MCP 配置文件路径**：以你的 MCP host 文档为准
- **检查 command 路径**：是否指向可执行文件 `bin/swarm-mcp`
- **重启 MCP host**：stdio MCP server 一般需要重启后才会重新拉起

### 2) 为什么找不到 `postIssueTaskMessage`？

- 默认 strict 模式下它会从 `tools/list` **隐藏**（不是删除）
- 期望的 worker 提问路径是 `askIssueTask`（阻塞直到 reply）
- 需要暴露全量工具：设置 `SWARM_MCP_STRICT=0`

### 3) `askIssueTask` 一直卡住/超时

- 正常行为：它会阻塞等待 lead 的 `replyIssueTaskMessage`
- 若超时：
  - 确认 lead 是否对同一个 `issue_id/task_id` 调用了 `replyIssueTaskMessage`
  - 检查 lead 是否在另一个 `SWARM_MCP_ROOT`（多项目隔离时容易配错）

### 4) `waitIssueTaskEvents` 没有返回事件

- 这是一个 long-poll 接口：
  - 没新事件时会在 `timeout_sec` 后返回空数组
  - 正常用法是维护一个游标：`after_seq` / `next_seq`

建议模式：

- 首次：`after_seq=0`
- 每次返回后：把 `after_seq` 更新为返回的 `next_seq`

### 5) 锁相关问题（lockFiles 失败 / 误以为死锁）

- `lockFiles` 失败通常表示：
  - 目标文件已被其他窗口持锁
  - 或 wait 超时（`wait_sec`）
- 排查方式：
  - `listLocks` 查看当前锁持有者与过期时间
  - 查看 `$SWARM_MCP_ROOT/trace/events.jsonl`（包含 lock_expired / lock_forced 等审计）

### 6) 为什么两个窗口“互相卡住”：lead 在 wait，worker 连 listIssueTasks 也像挂起

- **已解决的根因**：如果 lead/worker 复用同一个 server 进程（stdio transport），且 server 同步串行处理请求，则长轮询会把其它调用堵住。
- **当前实现**：server 已改为并发处理请求（避免 long-poll 堵住其它调用），并引入 session_id 作为窗口隔离令牌。
- **正确姿势**：两个窗口分别 `openSession`，并在每次 tools/call 的 arguments 中带上各自的 `session_id`。
- **排查**：若你看到 `session_id is required` 或 `unknown session_id`，说明该窗口未先 `openSession`，或复用了错误的 session_id。

## 主要工具（摘要）

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

## 测试

```bash
# 单元测试
go test ./...

# 集成测试
bash test.sh
```

## License

TBD
