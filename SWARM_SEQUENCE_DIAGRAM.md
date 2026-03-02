# Swarm MCP 三角色协作时序图


## 时序图使用说明

- **实线箭头**：同步调用，等待返回
- **虚线箭头**：异步通知，不等待
- **矩形框**：参与者角色
- **注释框**：阶段说明或系统内部处理
- **循环框**：重复操作（心跳续约、任务循环）

## 完整工作流时序图

```mermaid
sequenceDiagram
    participant L as Lead
    participant W as Worker
    participant A as Acceptor
    participant S as System

    %% 阶段 1: Issue 创建与初始化
    Note over L,S: 阶段 1: Issue 创建与初始化
    L->>S: createIssue(subject, docs)
    S-->>L: issue_id, status=open
    L->>S: createIssueTask(issue_id, spec)
    S-->>L: task_id, status=open

    %% 阶段 2: 任务领取与执行
    Note over W,S: 阶段 2: 任务领取与执行
    W->>S: registerWorker()
    S-->>W: worker_id
    W->>S: waitIssueTasks(issue_id, status=open)
    S-->>W: [task_list]k
    W->>S: claimIssueTask(issue_id, task_id, worker_id)
    S-->>W: task, status=in_progress
    
    W->>S: lockFiles(files, task_id)
    S-->>W: lease_id
    loop 心跳续约
        W->>S: heartbeat(lease_id)
        S-->>W: ok
    end
    W->>S: unlock(lease_id)
    S-->>W: ok

    W->>S: submitIssueTask(issue_id, task_id, artifacts)
    S-->>W: (阻塞等待评审)

    %% 阶段 3: Lead 评审与任务流转
    Note over L,S: 阶段 3: Lead 评审与任务流转
    L->>S: waitIssueTaskEvents(issue_id)
    S-->>L: submission_event
    L->>S: getNextStepToken(issue_id, task_id, worker_id, score)
    S-->>L: next_step_token, reserved_task
    L->>S: reviewIssueTask(issue_id, task_id, verdict, next_step_token)
    S-->>L: task, status=done
    S-->>W: review_result

    %% 阶段 4: Worker 领取下一个任务
    Note over W,S: 阶段 4: Worker 领取下一个任务
    W->>S: claimIssueTask(issue_id, next_task_id, next_step_token)
    S-->>W: next_task, status=in_progress

    %% 阶段 5: Q&A 与阻塞处理 (可选)
    Note over W,S: 阶段 5: Q&A 与阻塞处理
    alt Worker 遇到问题
        W->>S: askIssueTask(issue_id, task_id, question)
        S-->>W: (阻塞等待回复)
        S-->>L: question_notification
        L->>S: waitIssueTaskEvents(issue_id)
        S-->>L: question_event
        L->>S: replyIssueTaskMessage(issue_id, task_id, answer)
        S-->>W: answer_reply
    end

    %% 循环：继续任务执行
    loop 继续工作流
        W->>S: submitIssueTask(...)
        S-->>W: (阻塞等待)
        L->>S: waitIssueTaskEvents(...)
        S-->>L: submission_event
        L->>S: getNextStepToken(...)
        S-->>L: next_step_token
        L->>S: reviewIssueTask(...)
        S-->>L: task_done
        W->>S: claimIssueTask(..., next_step_token)
        S-->>W: next_task
    end

    %% 阶段 6: Issue 交付与验收
    Note over L,S: 阶段 6: Issue 交付与验收
    L->>S: listIssueTasks(issue_id, status=open)
    S-->>L: [] (所有任务完成)
    L->>S: submitDelivery(issue_id, artifacts, test_evidence)
    S-->>L: delivery_id, status=in_review
    
    A->>S: waitDeliveries(status=in_review)
    S-->>A: delivery_event
    A->>S: claimDelivery(delivery_id)
    S-->>A: delivery_details
    A->>A: 执行验收测试
    A->>S: reviewDelivery(delivery_id, verdict, verification)
    S-->>A: ok
    
    S-->>L: delivery_result
    L->>S: closeIssue(issue_id)
    S-->>L: issue, status=done
```

## next_step_token 机制详细时序图

```mermaid
sequenceDiagram
    participant L as Lead
    participant W as Worker
    participant S as System

    Note over L,S: 任务评审完成，生成下一任务令牌
    L->>S: getNextStepToken(issue_id, task_id, worker_id, completion_score)
    
    Note over S: 系统内部处理
    S->>S: 更新工人状态(积分+连续低分)
    S->>S: 计算基础难度(根据总积分)
    S->>S: 动态调整难度(连续低分降级)
    S->>S: 筛选候选任务(按难度)
    S->>S: 选择最适合任务(按积分排序)
    S->>S: 预留选中任务(2分钟TTL)
    S->>S: 生成next_step_token

    S-->>L: {next_step_token, next_step: {type: "claim_task", task_id: "task-2"}}

    Note over L,S: Lead 评审当前任务，关联令牌
    L->>S: reviewIssueTask(issue_id, task_id, verdict, next_step_token)
    S->>S: 验证token有效性
    S->>S: 更新任务状态(done/in_progress)
    S->>S: 标记token为attached=true
    S-->>L: review_complete

    Note over W,S: Worker 使用令牌领取预留任务
    W->>S: claimIssueTask(issue_id, task_id, next_step_token)
    S->>S: 验证任务预留状态
    S->>S: 检查预留时间未过期
    S->>S: 验证token未使用
    S->>S: 标记token为used=true
    S->>S: 清除任务预留状态
    S-->>W: task_claimed_successfully
```

## 文件锁机制时序图

```mermaid
sequenceDiagram
    participant W1 as Worker1
    participant W2 as Worker2
    participant S as System
    participant F as Files

    Note over W1,S: Worker1 申请文件锁
    W1->>S: lockFiles([file1, file2], task_id)
    S->>F: 检查文件锁定状态
    F-->>S: 文件未被锁定
    S->>S: 创建lease_id
    S->>F: 设置文件锁定(lease_id, TTL=120s)
    S-->>W1: lease_id

    Note over W1,S: Worker1 心跳续约
    loop 每30秒续约
        W1->>S: heartbeat(lease_id)
        S->>S: 延长lease TTL
        S-->>W1: ok
    end

    Note over W2,S: Worker2 尝试锁定相同文件
    W2->>S: lockFiles([file1], task_id)
    S->>F: 检查文件锁定状态
    F-->>S: 文件已被lease_id锁定
    S-->>W2: error: file_is_locked

    Note over W1,S: Worker1 完成工作释放锁
    W1->>S: unlock(lease_id)
    S->>F: 清除文件锁定
    S-->>W1: ok

    Note over W2,S: Worker2 重新尝试锁定
    W2->>S: lockFiles([file1], task_id)
    S->>F: 检查文件锁定状态
    F-->>S: 文件未被锁定
    S-->>W2: new_lease_id
```

## Q&A 阻塞处理时序图

```mermaid
sequenceDiagram
    participant W as Worker
    participant L as Lead
    participant S as System

    Note over W,S: Worker 遇到问题，任务阻塞
    W->>S: askIssueTask(issue_id, task_id, content="问题", kind="question")
    S->>S: 任务状态: in_progress → blocked
    S->>S: 创建TaskMessage实体
    S-->>W: (阻塞等待回复)
    S-->>L: question_notification

    Note over L,S: Lead 接收并回复问题
    L->>S: waitIssueTaskEvents(issue_id)
    S-->>L: question_event
    L->>S: replyIssueTaskMessage(issue_id, task_id, content="答案")
    S->>S: 任务状态: blocked → in_progress
    S->>S: 标记消息为已回复
    S-->>W: answer_reply

    Note over W,S: Worker 收到回复，继续工作
    W-->>S: (解除阻塞，继续执行)
```

## 异常处理时序图

```mermaid
sequenceDiagram
    participant W as Worker
    participant L as Lead
    participant S as System

    Note over W,S: 场景1: 任务租约过期
    W->>S: claimIssueTask(issue_id, task_id, worker_id)
    S-->>W: task, lease_expires_at=T+120s
    
    Note over S: 租约过期，系统自动清理
    S->>S: 检查过期任务
    S->>S: 任务状态: in_progress → open
    S->>S: 清除claimed_by信息
    
    Note over L,S: Lead 重置问题任务
    L->>S: resetIssueTask(issue_id, task_id, reason="需要重新设计")
    S->>S: 任务状态重置为open
    S->>S: 清除所有提交记录
    S-->>L: reset_complete

    Note over W,S: Worker 重新领取任务
    W->>S: claimIssueTask(issue_id, task_id, worker_id)
    S-->>W: task, status=in_progress

    Note over W,S: 场景2: next_step_token 失效
    W->>S: claimIssueTask(issue_id, task_id, next_step_token="expired_token")
    S->>S: 验证token状态
    S-->>W: error: invalid_next_step_token
    
    Note over L,S: Lead 重新生成token
    L->>S: getNextStepToken(issue_id, prev_task_id, worker_id, score)
    S-->>L: new_next_step_token
    
    Note over W,S: Worker 使用新token领取任务
    W->>S: claimIssueTask(issue_id, task_id, next_step_token="new_token")
    S-->>W: task_claimed_successfully
```

## 并发安全机制时序图

```mermaid
sequenceDiagram
    participant W1 as Worker1
    participant W2 as Worker2
    participant L as Lead
    participant S as System

    Note over S: 全局锁保护并发操作
    par Worker1 操作
        W1->>S: claimIssueTask(issue_id, task_id, worker_id)
    and Worker2 操作
        W2->>S: claimIssueTask(issue_id, task_id, worker_id)
    end
    
    Note over S: 系统串行化处理
    S->>S: 获取全局锁
    S->>S: 处理Worker1请求
    S-->>W1: task_claimed_successfully
    S->>S: 释放全局锁
    S->>S: 获取全局锁
    S->>S: 处理Worker2请求
    S-->>W2: error: task_already_claimed
    S->>S: 释放全局锁

    Note over L,S: Lead 同时评审多个任务
    par Lead 评审任务1
        L->>S: reviewIssueTask(issue_id, task1, verdict, token1)
    and Lead 评审任务2
        L->>S: reviewIssueTask(issue_id, task2, verdict, token2)
    end
    
    Note over S: 串行化评审处理
    S->>S: 获取全局锁
    S->>S: 处理任务1评审
    S-->>L: task1_review_complete
    S->>S: 释放全局锁
    S->>S: 获取全局锁
    S->>S: 处理任务2评审
    S-->>L: task2_review_complete
    S->>S: 释放全局锁
```

## 数据流状态转换图

```mermaid
stateDiagram-v2
    [*] --> IssueCreated: Lead创建Issue
    IssueCreated --> TaskOpen: Lead创建任务
    
    TaskOpen --> TaskInProgress: Worker领取任务
    TaskInProgress --> TaskBlocked: Worker提问
    TaskInProgress --> TaskSubmitted: Worker提交成果
    
    TaskBlocked --> TaskInProgress: Lead回复
    TaskSubmitted --> ReviewInProgress: Lead开始评审
    
    ReviewInProgress --> TaskDone: 评审通过
    ReviewInProgress --> TaskInProgress: 评审拒绝
    
    TaskDone --> NextTaskReserved: 生成next_step_token
    NextTaskReserved --> TaskOpen: Worker领取下一任务
    
    TaskOpen --> AllTasksDone: 所有任务完成
    AllTasksDone --> DeliveryInReview: Lead提交交付
    DeliveryInReview --> DeliveryApproved: Acceptor验收通过
    DeliveryApproved --> IssueClosed: Lead关闭Issue
    IssueClosed --> [*]
```

## 关键时间节点说明

| 时间节点 | 操作 | 超时设置 | 说明 |
|---------|------|----------|------|
| T+0s | Worker领取任务 | - | 任务状态变为in_progress |
| T+30s | 第一次心跳 | 30s间隔 | Worker必须开始续约 |
| T+60s | 第二次心跳 | 30s间隔 | 继续保持文件锁 |
| T+90s | 第三次心跳 | 30s间隔 | 最后一次正常续约 |
| T+120s | 租约过期 | 120s TTL | 文件锁自动释放 |
| T+120s | 任务预留过期 | 120s TTL | next_step_token预留任务释放 |
| T+3600s | 评审等待 | 3600s默认 | Worker等待Lead评审的超时 |
| T+3600s | Q&A等待 | 3600s默认 | Worker等待Lead回复的超时 |


