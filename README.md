# Swarm MCP (Issue-Centric)

一个用于多窗口多智能体协作的本地 MCP Server（stdio 模式），可接入任意兼容 MCP 的客户端（MCP host）。

[English](README.en.md) | **中文**

本项目采用 **issue-centric** 协作模型：

- **Issue**：一个主问题（工作池容器）
- **Task**：可被任意 worker 领取的工作单元
- 通过 **事件流**（long-poll）进行 lead/worker 协作
- 通过 **文件锁（租约）** 防止并发改文件冲突
- 通过 **文档库（docs library）** 在 lead/worker 之间传递上下文

## 特性

- Issue 池：任务以池的形式散播，worker 自由认领
- Worker 身份登记：worker 领取任务后可追踪“谁做了什么”
- 角色代码（强安全约束）：通过环境变量配置角色代码，防止跨角色工具调用
- 工号绑定（身份约束）：worker 必须使用工号（worker_id）领取和操作任务，防止操作他人任务
- 事件流：
  - `waitIssueTaskEvents`：issue 级 select-like
- 阻塞式问答：`askIssueTask`（worker 提问后阻塞等待 lead reply）
- Task 状态机联动：
  - `kind=question|blocker` → task 自动置为 `blocked`
  - `kind=reply` → task 自动解除回 `in_progress`

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

### 角色代码（Role Code）

为了防止跨角色工具调用，Swarm MCP 支持基于环境变量的 **role_code** 机制：

- **SWARM_MCP_ROLE_CODE_LEAD**：Lead 角色代码
- **SWARM_MCP_ROLE_CODE_WORKER**：Worker 角色代码  
- **SWARM_MCP_ROLE_CODE_ACCEPTOR**：Acceptor 角色代码

当配置了对应角色的代码后：

- `tools/list` 会自动在所有工具的 input schema 中注入 `role_code`（required）
- `tools/call` 必须携带正确的 `role_code`，否则拒绝调用
- 未配置代码的角色：不注入、不校验（兼容调试模式）

### Worker 工号绑定（身份约束）

Worker 在操作任务时必须使用 **工号（worker_id）** 进行身份绑定：

- **claimIssueTask** 必须传 `worker_id`（工号），任务被标记为该工号领取
- **submitIssueTask**、**askIssueTask**、**postIssueTaskMessage** 必须使用同一个 `worker_id`，服务端校验 `task.claimed_by == worker_id`
- **lockFiles**（带 `task_id` 时）必须同时传 `issue_id` 和 `worker_id`，服务端校验任务归属
- **heartbeat** / **unlock** 必须使用 `worker_id`，服务端校验 `lease.owner == worker_id`

这确保：一个 worker 只能操作自己领取的任务，无法冒领或操作他人任务。

### 三角色二进制（推荐）

为了减少单个模型的工具面、避免角色职责混乱，推荐在 MCP host 层配置三个不同二进制：

- `swarm-mcp-lead`
- `swarm-mcp-worker`
- `swarm-mcp-acceptor`

说明：

- 服务端内置 **role allowlist**：`tools/list` 仅返回该角色允许的工具；`tools/call` 调用越权工具会直接报错

#### MCP host 配置示例（mcp_config.json）

下面给出一套推荐的 `disabledTools` 清单：目标是减少信息膨胀，并进一步阻止模型走“非阻塞消息/写文档/锁”等容易引发混乱的工具。

```json
{
  "swarm-mcp-lead": {
    "command": "/path/to/bin/swarm-mcp-lead",
    "disabledTools": [
      "closeIssue",
      "reopenIssue",
      "forceUnlock"
    ]
  },
  "swarm-mcp-worker": {
    "command": "/path/to/bin/swarm-mcp-worker",
    "disabledTools": [
      "postIssueTaskMessage",
      "listLocks"
    ]
  },
  "swarm-mcp-acceptor": {
    "command": "/path/to/bin/swarm-mcp-acceptor",
    "disabledTools": [
      "getIssue",
      "getIssueTask",
      "listSharedDocs",
      "listIssueDocs",
      "listTaskDocs",
      "readSharedDoc",
      "readIssueDoc",
      "readTaskDoc"
    ]
  }
}
```

### ID / 会话机制（为什么要显式传 issue_id/task_id）

- MCP 工具调用默认是“显式参数”风格：本 server 不会为你维护“当前 issue/task 上下文”
- 你忘了 ID 时的恢复方式：
  - `listIssues(status=...)` / `listOpenedIssues` / `getIssue`
  - `listIssueTasks(issue_id, status=...)` / `listIssueOpenedTasks(issue_id)` / `getIssueTask`

另外：本 server 引入了 **session_id（强约束）** 作为“调用者语义会话令牌”（类似 cookie 的作用）：

- 你需要先通过 `session-mcp.upsertSemanticSession` 获取一个有效的 `session_id`
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

### 推荐使用姿势（两阶段注入）

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
现在请你将开发清单写入文档(目录格式为 ./ai-issue-doc/issue-xxx-todo.md, 请替换 xxx 为该 issue 的数字)， 用于后续逐一验收

然后进入协作阶段：这里有一个叫 swarm-mcp-lead 的协作 MCP Server
你需要：
1) 基于你刚才的分析，重新规划一次（允许重拆分/重排序）以适配多人同时协作
2) 然后用 swarm-mcp-lead 自主完成：创建 issue、拆分 task，并把 issue 散播出去让各 worker 自行领取 task

[角色]
你是 Lead，role_code 为 123。

[协作规则]
- 推荐流程：createIssue -> createIssueTask -> waitIssueTaskEvents -> review/reply
- 未经明确要求：你必须只做 lead（拆分/答疑/验收/事件循环），不要自己下场实现需求、不要去改 worker 的目标代码文件
- 分析 issue 的实际复杂度与规模，合理拆分任务数量(一般为2-5个)，且能实现并行开发避免文件冲突(虽然 worker 们能调用文件锁)
- 调用 waitIssueTaskEvents 接收到事件后务必严苛审视，仔细推理分析并处理该事件，严格判断该 task 实现的正确性，处理完后需要继续不断调用 waitIssueTaskEvents
- reply/review 请务必匹配其 issue_id, task_id 及 worker_id
- 等待 worker 们完成所有 tasks 后你需要做整体测试以及交付
- **强约束** 务必遵循接口工具返回的 next_actions
```

##### Worker 提示词

```text
你当前处于 MCP 协作模式，可以调用 swarm-mcp-worker 提供的工具来完成任务

[角色]
你是 Worker，role_code 为 153。

[协作规则]
- 领取任务：swarm-mcp-worker.waitIssues(status=open) -> waitIssueTasks(status=open) -> registerWorker ->  claimIssueTask，任务开始前务必调用 claimIssueTask
- 若无任何 open 状态 的 issues 或 tasks 时，请务必调用 waitIssues 或 waitIssueTasks
- 修改代码前必须加锁：lockFiles(files=[...])，没有有效 lockFiles 锁，不要修改任何文件，持锁期间每 ~30s 续租：heartbeat，每完成一个文件的修改后必须释放该文件锁：unlock
- 开发中有任务不确定/或信息不足时请务必使用 askIssueTask(kind=question|blocker) 获取 lead 的决策，然后继续推进
- 你可访问该项目其他未在 task 中包含的文档, 你拥有该项目所有读写权限
- 领取任务后务必查阅其有关的所有文档与信息，并仔细推理分析每个细节，谨慎周密地完成该任务
- 文档需通过远端下载
- **强约束** 完成任务后提交：submitIssueTask，根据返回的 next_actions 继续进行下一步
- **强约束** 当所有 tasks 被完成后继续调用 waitIssues 或 waitIssueTasks 直至没有任何 open 状态的 issue
- **强约束** 务必遵循接口工具返回的 next_actions
```

##### 验收（Acceptor）提示词

```text
你当前处于 MCP 协作模式，可以调用 swarm-mcp-acceptor 提供的工具来完成验收

[角色]
你是 验收（Acceptor），role_code 为 123153。

[协作规则]
- 验收流程：swarm-mcp-acceptor.waitDeliveries(status=open) -> claimDelivery -> getIssueAcceptanceBundle -> reviewDelivery
- **强约束** 你必须分析整个代码库以及已知的所有文档信息，务必严苛审视及充分推理分析，严格判断该 issue 实现的正确性
- 除了推理分析，你还必须进行实际验收：执行其一键测试脚本保证运行成功; 如果需求包含界面跳转则必须调用 playwright-enhanced-mcp 做一次全流程测试，跑通整个链路(不仅是点击与查看, 需要在浏览器录入/修改/删除数据实测整个流程)并注意每个需求的细节(包括按钮/样式/布局/交互等任何需求内的界面问题)
- 当遇到必须由用户主动介入的情况你才能停下来向用户发起询问, 否则请务必完成协作流程直至验收成功
- 当验收成功后继续调用 waitDeliveries 直至没有任何 open 状态的 issue
- **强约束** 务必遵循接口工具返回的 next_actions
```

##### 最小流程示例（端到端）

###### 阻塞/挂起语义速查

说明：本 server 的若干工具会“挂起”（阻塞）以配合被动事件循环与严格协作。

| 工具 | 是否挂起 | 何时返回 | 默认/固定超时 | 备注 |
| --- | --- | --- | --- | --- |
| `waitIssueTaskEvents(issue_id)` | 是 | 仅当出现 `question/blocker` 或 `issue_task_submitted` 信号 | 固定 3600s | Lead 被动事件循环；一次最多返回 1 条 signal；忽略其它事件并继续挂起 |
| `submitDelivery(issue_id, ...)` | 是 | 交付后，直到验收方 `reviewDelivery` 返回结论 | 默认 3600s（可传） | Lead 交付给验收方；必须提供结构化 `artifacts`（至少 `test_result`/`test_cases`/`changed_files`/`reviewed_refs`） |
| `waitDeliveries(status=open, ...)` | 是 | delivery 池中出现新的 open delivery（按数量增长触发） | 默认 3600s（可传） | 验收方被动等待交付；返回 deliveries 列表（通常取第一条） |
| `submitIssueTask(issue_id, task_id, ...)` | 是 | 提交后，直到 lead `reviewIssueTask` 产生 `reviewed/resolved` 事件 | 固定 3600s | 提交必须携带结构化 `artifacts`；用于防止 worker 提交后立即结束对话 |
| `askIssueTask(issue_id, task_id, ...)` | 是 | lead `replyIssueTaskMessage` 回复后 | 默认 3600s（可传） | 会先发出 `question/blocker` 再等待 reply |
| `lockFiles(...)` | 可能 | 拿到锁即返回；若被占用则等待到 `wait_sec` | `wait_sec` | 不会无限挂起，超时会失败返回 |
| `reopenIssue(issue_id, ...)` | 否 | 调用成功立即返回 | - | 仅当 issue 已 `done/canceled` 时可调用；用于重新开启并触发再次审查 |

另外：同一 issue 内的 `task_id` 为递增序列：`task-1`、`task-2`…（不会与其它 issue 冲突）。

1. **Lead 创建工作池**
   - `createIssue(subject="...", description="...", user_issue_doc={name:"user", content:"..."}, lead_issue_doc={name:"lead", content:"..."}, user_other_docs=[{name:"context", content:"..."}])`
   - `createIssueTask(issue_id, subject="...", description="...", difficulty="easy|medium|focus", context_task_ids=[...], suggested_files=[...], spec={name:"spec", split_from:"...", split_reason:"...", impact_scope:"...", context_task_ids:[...], goal:"...", rules:"...", constraints:"...", conventions:"...", acceptance:"..."})`

2. **Worker 领取并实现**
   - （可选）当 lead 尚未创建 issue 时，你可以先调用 `waitIssues(timeout_sec=3600)` 阻塞等待 issue 出现
   - （可选）当你已知道 `issue_id` 但 lead 尚未创建 tasks 时，你可以调用 `waitIssueTasks(issue_id, timeout_sec=3600)` 阻塞等待 task 出现
   - `listIssueOpenedTasks(issue_id)`
   - `claimIssueTask(issue_id, task_id)`（若该 task 被 lead 预留，则必须带 `next_step_token`）
   - `lockFiles(task_id, files=["path/to/file.go"], ttl_sec=120, wait_sec=60)`
   - （编码；期间 `heartbeat(lease_id)`）
   - `unlock(lease_id)`
   - `submitIssueTask(issue_id, task_id, artifacts={summary:"...", changed_files:[...], diff:"...", links:[...], test_cases:[...], test_result:"passed|failed", test_output:"..."})` -> 挂起等待 lead review

   Worker 侧建议把整个过程当作一个循环执行：

   - `submitIssueTask` 返回后不要结束对话；它返回的结果里会包含 `next_actions`，你应继续执行其中的动作（例如 `listIssueOpenedTasks` / `claimIssueTask`）来领取下一项工作。
   - 重复「领取 -> 锁文件 -> 实现 -> 提交」直到：
     - `listIssueOpenedTasks(issue_id)` 为空（没有可领取任务），或
     - lead 明确结束该 issue / 不再派发任务。

3. **Lead 验收或打回**
   - `waitIssueTaskEvents(issue_id)` -> 收到 submitted/question/blocker
   - `getNextStepToken(issue_id, task_id, worker_id, completion_score=1|2|5)` -> 服务端自动挑选并预留 next task，返回 `next_step_token`
   - `reviewIssueTask(issue_id, task_id, verdict="approved|rejected", feedback="...", completion_score=1|2|5, artifacts={review_summary:"...", reviewed_refs:[...]}, feedback_details=[...], next_step_token="...")`

4. **Q&A**
   - Worker：`askIssueTask(issue_id, task_id, kind="question", content="...", timeout_sec=3600)`
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
        "SWARM_MCP_ROOT": "/Users/you/.swarm-mcp/<project_key>"
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

- `SWARM_MCP_ROLE`（仅 `swarm-mcp` legacy 二进制有效）
  - 可选值：`lead` | `worker` | `acceptor`
  - 不设置时：暴露全量工具（full-access debug 模式），stderr 会打印 WARNING
  - 推荐生产用途改用三角色专用二进制（`swarm-mcp-lead` 等），无需此变量

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

### 核心角色流程

**Lead窗口**：

1. 创建issue（`createIssue`）
2. 分解tasks（`createIssueTask`）
3. 等待tasks完成（`waitIssueTasks`、`listIssueTasks`）—— wait默认返回status=open的任务
4. 交付issue（`submitDelivery`，阻塞至验收）
5. 监控deliveries（`waitDeliveries`、`listOpenedDeliveries`）—— wait默认返回status=open的交付

### Worker窗口

1. 等待tasks（`waitIssueTasks`）—— 返回status=open的任务，若存在则立即返回
2. 领取task（`claimIssueTask`）—— 转为in_progress状态
3. 提交work result（`submitIssueTask`，阻塞至lead审核）
4. 续约（`extendIssueTaskLease`）—— 防止lease过期回退到open

### 回收规则（做了什么）

- issue 过期：`open|in_progress` -> `canceled`，并追加事件 `issue_expired`
- task 过期：`in_progress|blocked|submitted` -> `open`（可重新领取），并追加事件 `issue_task_expired`

### Wait工具语义

所有`wait*`工具使用统一的**立即返回+状态过滤**语义：

- **立即返回**：若存在匹配status的对象，立即返回（不等待增量）
- **状态过滤**：`status`参数默认为`open`，可指定其他状态（in_progress/done等）
- **阻塞等待**：若无匹配对象，则长轮询直到出现或超时
- **超时**：`timeout_sec`默认3600秒，**最小值3600秒**（传入小于3600s的值会被自动提升到3600s）
- **限制数量**：`limit`参数默认50，控制返回数量上限
- **跨进程安全**：基于文件系统轮询，支持多进程协作

> **注意**：所有阻塞接口的`timeout_sec`参数都有3600秒（1小时）的最小限制。这是为了确保协作的持续性，防止AI故意传入较短参数以提早结束会话。可通过环境变量`SWARM_MCP_DEFAULT_TIMEOUT_SEC`自定义最小值。
> **韧性机制**：对会在服务端阻塞等待的流程（例如 `submitIssueTask`/`askIssueTask`/`claimDelivery`），服务端会在进入阻塞前（或状态切换时）确保对应对象的 lease 覆盖至少 `SWARM_MCP_DEFAULT_TIMEOUT_SEC`，避免阻塞等待期间对象因 lease 过期被回收/回滚导致会话挂起或协作中断。

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

### 2) `askIssueTask` 一直卡住/超时

- 正常行为：它会阻塞等待 lead 的 `replyIssueTaskMessage`
- 若超时：
  - 确认 lead 是否对同一个 `issue_id/task_id` 调用了 `replyIssueTaskMessage`
  - 检查 lead 是否在另一个 `SWARM_MCP_ROOT`（多项目隔离时容易配错）

### 3) `waitIssueTaskEvents` 没有返回事件

- 这是一个 signals-only long-poll 接口：
  - 若没有新的 signal，会在超时后返回空数组
  - **不需要也不支持传入 `after_seq`**：服务端会为 lead 窗口按 `(issue_id, member_id)` 自动持久化/恢复 cursor
  - 正常用法是：处理完返回的事件后，继续调用 `waitIssueTaskEvents(issue_id)`

### 4) 锁相关问题（lockFiles 失败 / 误以为死锁）

- `lockFiles` 失败通常表示：
  - 目标文件已被其他窗口持锁
  - 或 wait 超时（`wait_sec`）
- 排查方式：
  - `listLocks` 查看当前锁持有者与过期时间
  - 查看 `$SWARM_MCP_ROOT/trace/events.jsonl`（包含 lock_expired / lock_forced 等审计）

### 5) 为什么两个窗口“互相卡住”：lead 在 wait，worker 连 listIssueTasks 也像挂起

- **已解决的根因**：如果 lead/worker 复用同一个 server 进程（stdio transport），且 server 同步串行处理请求，则长轮询会把其它调用堵住。
- **当前实现**：server 已改为并发处理请求（避免 long-poll 堵住其它调用），并引入 session_id 作为窗口隔离令牌。
- **正确姿势**：两个窗口分别通过 `session-mcp.upsertSemanticSession` 获取各自的 `session_id`，并在每次 tools/call 的 arguments 中带上该 `session_id`。
- **排查**：若你看到 `session_id is required` 或 `invalid semantic session`，说明该窗口未先获取有效的 semantic session id，或复用了错误的 session_id。

## 主要工具（摘要）

- Issue / Task
  - `createIssue`, `listIssues`（支持 status/subject_contains/分页/排序）, `listOpenedIssues`, `getIssue`
  - `createIssueTask`, `listIssueTasks`（支持 status/subject_contains/claimed_by/submitter/分页/排序）, `listIssueOpenedTasks`, `getIssueTask`
  - `claimIssueTask`, `submitIssueTask`, `reviewIssueTask`
  - `waitIssueTaskEvents`
  - `askIssueTask`, `replyIssueTaskMessage`
- Docs
  - `writeSharedDoc`, `readSharedDoc`, `listSharedDocs`
  - `writeIssueDoc`, `readIssueDoc`, `listIssueDocs`
  - `writeTaskDoc`, `readTaskDoc`, `listTaskDocs`
- Worker
  - `registerWorker`, `listWorkers`, `getWorker`, `myProfile`
- Locks
  - `lockFiles`, `heartbeat`, `unlock`, `listLocks`, `forceUnlock`

## 测试

```bash
# 单元测试
go test ./...

# 集成测试（默认启用 role_code 和工号机制）
bash test.sh

# 注意：test.sh 默认使用 role_code 和工号机制
# 如需调试，可修改 .env 或注释相关环境变量
```

## License

TBD
