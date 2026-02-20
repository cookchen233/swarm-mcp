package swarm

import (
	"fmt"
	"strings"
	"time"
)

// PostTaskMessage creates a TaskMessage entity and pushes it to the lead inbox.
// kind must be "question" or "blocker". Returns a synthetic IssueEvent for API compat.
func (s *IssueService) PostTaskMessage(issueID, taskID, actor, kind, content, refs string) (*IssueEvent, error) {
	if issueID == "" || taskID == "" {
		return nil, fmt.Errorf("issue_id and task_id are required")
	}
	if actor == "" {
		actor = "worker"
	}
	if kind == "" {
		kind = "question"
	}

	var ev *IssueEvent
	err := s.store.WithLock(func() error {
		task, err := s.loadTaskLocked(issueID, taskID)
		if err != nil {
			return err
		}
		if kind != "reply" {
			if task.ClaimedBy == "" {
				return fmt.Errorf("task '%s' is not claimed", taskID)
			}
			if strings.TrimSpace(task.ClaimedBy) != strings.TrimSpace(actor) {
				return fmt.Errorf("task '%s' is not claimed by actor", taskID)
			}
		}

		// Create the TaskMessage entity.
		msg, err := s.createTaskMessageLocked(issueID, taskID, actor, kind, content, refs)
		if err != nil {
			return err
		}

		// State machine: question/blocker → blocked.
		if (kind == "question" || kind == "blocker") && task.Status == IssueTaskInProgress {
			task.Status = IssueTaskBlocked
			task.UpdatedAt = NowStr()
			if err := s.store.WriteJSON(s.store.Path("issues", issueID, "tasks", task.ID+".json"), task); err != nil {
				return err
			}
		}

		// Push to lead inbox.
		if _, err := s.pushToLeadInboxLocked(issueID, taskID, kind, msg.ID, actor); err != nil {
			return err
		}

		// Append audit event.
		e := IssueEvent{
			Type:      EventIssueTaskMessage,
			IssueID:   issueID,
			TaskID:    taskID,
			Actor:     actor,
			Kind:      kind,
			Detail:    content,
			Refs:      refs,
			MessageID: msg.ID,
			Timestamp: NowStr(),
		}
		seq, err := s.appendEventLockedWithSeq(issueID, &e)
		if err != nil {
			return err
		}
		e.Seq = seq
		ev = &e
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.bump(issueID)
	return ev, nil
}

// ReplyTaskMessage replies to a specific TaskMessage by messageID, or the oldest open message if empty.
// This is the lead→worker reply path.
func (s *IssueService) ReplyTaskMessage(issueID, taskID, actor, messageID, content, refs string) (*IssueEvent, error) {
	if issueID == "" || taskID == "" {
		return nil, fmt.Errorf("issue_id and task_id are required")
	}
	if actor == "" {
		actor = "lead"
	}

	var ev *IssueEvent
	err := s.store.WithLock(func() error {
		task, err := s.loadTaskLocked(issueID, taskID)
		if err != nil {
			return err
		}

		// Find the message to reply to.
		msg, err := s.resolveMessageForReply(issueID, taskID, messageID)
		if err != nil {
			return err
		}

		// Update the message entity.
		repliedMsg, err := s.replyTaskMessageLocked(issueID, msg.ID, actor, content, refs)
		if err != nil {
			return err
		}

		// Ack the lead inbox item for this message.
		s.ackLeadInboxByRefLocked(issueID, msg.ID)

		// Push reply to worker inbox.
		if task.ClaimedBy != "" {
			_, _ = s.pushToWorkerInboxLocked(issueID, task.ClaimedBy, taskID, InboxTypeReply, msg.ID, actor)
		}

		// State machine: reply → unblock back to in_progress.
		if task.Status == IssueTaskBlocked {
			task.Status = IssueTaskInProgress
			task.UpdatedAt = NowStr()
			if err := s.store.WriteJSON(s.store.Path("issues", issueID, "tasks", task.ID+".json"), task); err != nil {
				return err
			}
		}

		// Append audit event.
		e := IssueEvent{
			Type:      EventIssueTaskMessage,
			IssueID:   issueID,
			TaskID:    taskID,
			Actor:     actor,
			Kind:      "reply",
			Detail:    content,
			Refs:      repliedMsg.Refs,
			MessageID: msg.ID,
			Timestamp: NowStr(),
		}
		seq, err := s.appendEventLockedWithSeq(issueID, &e)
		if err != nil {
			return err
		}
		e.Seq = seq
		ev = &e
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.bump(issueID)
	return ev, nil
}

// AskIssueTask creates a TaskMessage entity and blocks until the lead replies.
// Returns a map with "question" (event) and "reply" (event) on success.
func (s *IssueService) AskIssueTask(issueID, taskID, actor, kind, content, refs string, timeoutSec int) (map[string]any, error) {
	if kind == "" {
		kind = "question"
	}
	if kind != "question" && kind != "blocker" {
		return nil, fmt.Errorf("kind must be question or blocker")
	}
	timeoutSec = s.normalizeTimeoutSec(timeoutSec)

	qEvent, err := s.PostTaskMessage(issueID, taskID, actor, kind, content, refs)
	if err != nil {
		return nil, err
	}
	messageID := qEvent.MessageID

	// Extend task lease to cover the wait period.
	_ = s.store.WithLock(func() error {
		task, err := s.loadTaskLocked(issueID, taskID)
		if err != nil {
			return nil
		}
		if actor != "" && task.ClaimedBy == actor {
			nowMs := time.Now().UnixMilli()
			minLeaseMs := nowMs + int64(s.defaultTimeoutSec)*1000
			if task.LeaseExpiresAtMs < minLeaseMs {
				task.LeaseExpiresAtMs = minLeaseMs
				task.UpdatedAt = NowStr()
				_ = s.store.WriteJSON(s.store.Path("issues", issueID, "tasks", task.ID+".json"), task)
			}
		}
		return nil
	})

	// Poll the TaskMessage entity until it has a reply (entity-based, not event-scanning).
	repliedMsg, err := s.pollMessageReply(issueID, messageID, timeoutSec)
	if err != nil {
		return nil, err
	}

	replyEvent := IssueEvent{
		Type:      EventIssueTaskMessage,
		IssueID:   issueID,
		TaskID:    taskID,
		Actor:     repliedMsg.ReplyBy,
		Kind:      "reply",
		Detail:    repliedMsg.ReplyContent,
		Refs:      repliedMsg.Refs,
		MessageID: messageID,
		Timestamp: repliedMsg.RepliedAt,
	}

	return map[string]any{
		"question":   qEvent,
		"reply":      replyEvent,
		"message_id": messageID,
	}, nil
}

// WaitIssueTaskEvents blocks until a lead inbox item is available (submission or question/blocker).
// Uses the inbox queue for reliable single-consumer delivery instead of event cursor scanning.
// Returns up to 1 signal event. timeoutSec <= 0 defaults to service default.
func (s *IssueService) WaitIssueTaskEvents(issueID, actor string, afterSeq int64, timeoutSec, limit int) ([]IssueEvent, int64, error) {
	if issueID == "" {
		return nil, afterSeq, fmt.Errorf("issue_id is required")
	}
	if !s.store.Exists("issues", issueID, "issue.json") {
		return nil, afterSeq, fmt.Errorf("issue '%s' not found", issueID)
	}
	s.SweepExpired()
	if actor == "" {
		actor = "lead"
	}
	var issue Issue
	if err := s.store.ReadJSON(s.store.Path("issues", issueID, "issue.json"), &issue); err != nil {
		return nil, afterSeq, err
	}
	if issue.Status == IssueDone || issue.Status == IssueCanceled {
		return []IssueEvent{}, afterSeq, nil
	}
	tasks, err := s.ListTasks(issueID, "")
	if err != nil {
		return nil, afterSeq, err
	}
	if len(tasks) == 0 {
		return []IssueEvent{}, afterSeq, nil
	}
	allDone := true
	for _, t := range tasks {
		if t.Status != IssueTaskDone && t.Status != IssueTaskCanceled {
			allDone = false
			break
		}
	}
	if allDone {
		return []IssueEvent{}, afterSeq, nil
	}
	timeoutSec = s.normalizeTimeoutSec(timeoutSec)

	// Sweep stale inbox claims before polling.
	s.sweepInboxClaims(issueID)

	// Claim the oldest pending inbox item (blocks until found or timeout).
	item, err := s.claimLeadInboxBlocking(issueID, actor, timeoutSec)
	if err != nil {
		return nil, afterSeq, err
	}
	if item == nil {
		// Timeout with no items.
		return []IssueEvent{}, afterSeq, nil
	}

	// Convert inbox item to an event-shaped response for API compatibility.
	mat := s.materializeInboxItem(issueID, item)
	ev := IssueEvent{
		Type:         fmt.Sprint(mat["type"]),
		IssueID:      issueID,
		TaskID:       fmt.Sprint(mat["task_id"]),
		Actor:        fmt.Sprint(mat["actor"]),
		Kind:         fmt.Sprint(mat["kind"]),
		Detail:       fmt.Sprint(mat["detail"]),
		Refs:         fmt.Sprint(mat["refs"]),
		Timestamp:    fmt.Sprint(mat["timestamp"]),
		SubmissionID: fmt.Sprint(mat["submission_id"]),
		MessageID:    fmt.Sprint(mat["message_id"]),
	}
	if sa, ok := mat["submission_artifacts"]; ok {
		if saTyped, ok2 := sa.(SubmissionArtifacts); ok2 {
			ev.SubmissionArtifacts = &saTyped
		}
	}
	// Use -1 as the seq since we're no longer event-seq based.
	ev.Seq = -1

	return []IssueEvent{ev}, afterSeq, nil
}
