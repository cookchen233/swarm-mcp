package swarm

import (
	"fmt"
	"time"
)

func (s *IssueService) PostTaskMessage(issueID, taskID, actor, kind, content, refs string) (*IssueEvent, error) {
	if issueID == "" || taskID == "" {
		return nil, fmt.Errorf("issue_id and task_id are required")
	}
	if actor == "" {
		actor = "worker"
	}
	if kind == "" {
		kind = "message"
	}

	var ev *IssueEvent
	err := s.store.WithLock(func() error {
		// Ensure task exists
		task, err := s.loadTaskLocked(issueID, taskID)
		if err != nil {
			return err
		}

		// State machine linkage:
		// - question/blocker => blocked
		// - reply => unblock back to in_progress
		switch kind {
		case "question", "blocker":
			if task.Status == IssueTaskInProgress {
				task.Status = IssueTaskBlocked
				task.UpdatedAt = NowStr()
				if err := s.store.WriteJSON(s.store.Path("issues", issueID, "tasks", task.ID+".json"), task); err != nil {
					return err
				}
			}
		case "reply":
			if task.Status == IssueTaskBlocked {
				task.Status = IssueTaskInProgress
				task.UpdatedAt = NowStr()
				if err := s.store.WriteJSON(s.store.Path("issues", issueID, "tasks", task.ID+".json"), task); err != nil {
					return err
				}
			}
		}

		e := IssueEvent{
			Type:      EventIssueTaskMessage,
			IssueID:   issueID,
			TaskID:    taskID,
			Actor:     actor,
			Kind:      kind,
			Detail:    content,
			Refs:      refs,
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

func (s *IssueService) ReplyTaskMessage(issueID, taskID, actor, content, refs string) (*IssueEvent, error) {
	return s.PostTaskMessage(issueID, taskID, actor, "reply", content, refs)
}

// WaitTaskEvents blocks until there are new events for a specific task after the given seq.
// It returns up to limit events (default 20). If timeoutSec <= 0, defaults to 600.
func (s *IssueService) WaitTaskEvents(issueID, taskID string, afterSeq int64, timeoutSec, limit int) ([]IssueEvent, int64, error) {
	if issueID == "" || taskID == "" {
		return nil, afterSeq, fmt.Errorf("issue_id and task_id are required")
	}
	if timeoutSec <= 0 {
		timeoutSec = 600
	}
	if limit <= 0 {
		limit = 20
	}

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	poll := 200 * time.Millisecond

	for {
		events, nextSeq, err := s.readTaskEventsAfter(issueID, taskID, afterSeq, limit)
		if err != nil {
			return nil, afterSeq, err
		}
		if len(events) > 0 {
			return events, nextSeq, nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return []IssueEvent{}, afterSeq, nil
		}

		if remaining < poll {
			time.Sleep(remaining)
		} else {
			time.Sleep(poll)
		}
	}
}

// AskIssueTask posts a question/blocker message and blocks until a reply is received.
// It returns the question event and the reply event (kind=reply).
func (s *IssueService) AskIssueTask(issueID, taskID, actor, kind, content, refs string, timeoutSec int) (map[string]any, error) {
	if kind == "" {
		kind = "question"
	}
	if kind != "question" && kind != "blocker" {
		return nil, fmt.Errorf("kind must be question or blocker")
	}
	if timeoutSec <= 0 {
		timeoutSec = 600
	}

	q, err := s.PostTaskMessage(issueID, taskID, actor, kind, content, refs)
	if err != nil {
		return nil, err
	}

	// Wait for a reply after the question seq.
	after := q.Seq
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("timeout waiting for reply")
		}
		// Shorter polling slices so we can respect deadline precisely.
		slice := 5
		if remaining < 5*time.Second {
			slice = int(remaining.Seconds())
			if slice <= 0 {
				slice = 1
			}
		}

		events, nextSeq, err := s.WaitTaskEvents(issueID, taskID, after, slice, 50)
		if err != nil {
			return nil, err
		}
		for _, ev := range events {
			if ev.Type == EventIssueTaskMessage && ev.Kind == "reply" {
				return map[string]any{
					"question": q,
					"reply":    ev,
					"next_seq": nextSeq,
				}, nil
			}
		}
		after = nextSeq
	}
}

// WaitIssueTaskEvents blocks until there are new signal events for this issue.
// Signals are:
// 1) worker question/blocker messages (issue_task_message kind=question|blocker)
// 2) task submitted events (issue_task_submitted)
//
// Cursor semantics:
// - If afterSeq >= 0: use it as the explicit cursor.
// - If afterSeq < 0: auto-resume from a persisted per-(issue,actor) cursor. If missing, tail to the current end.
//
// It returns up to limit events (default 20). If timeoutSec <= 0, defaults to 600.
func (s *IssueService) WaitIssueTaskEvents(issueID, actor string, afterSeq int64, timeoutSec, limit int) ([]IssueEvent, int64, error) {
	if issueID == "" {
		return nil, afterSeq, fmt.Errorf("issue_id is required")
	}
	s.SweepExpired()
	if actor == "" {
		actor = "lead"
	}
	if timeoutSec <= 0 {
		timeoutSec = 600
	}
	if limit <= 0 {
		limit = 20
	}

	cursorPath := s.store.Path("issues", issueID, "cursors", actor+".json")
	if afterSeq < 0 {
		// Try resume
		var c issueCursor
		if err := s.store.ReadJSON(cursorPath, &c); err == nil {
			afterSeq = c.AfterSeq
		} else {
			// No cursor found: start from the beginning.
			// Use -1 so we include seq=0 events.
			// This avoids missing already-emitted signal events when a lead starts waiting late.
			afterSeq = -1
		}
	}

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	// Polling interval: keep it small so cross-process submissions are picked up quickly,
	// but not too small to avoid busy-wait.
	poll := 200 * time.Millisecond

	for {
		s.SweepExpired()
		events, nextSeq, err := s.readEventsAfter(issueID, afterSeq, limit)
		if err != nil {
			return nil, afterSeq, err
		}
		if len(events) > 0 {
			// Signals-only long-poll: only return when there is actionable input.
			// Allowed return cases:
			// 1) Worker asks a question/blocker (issue_task_message kind=question|blocker)
			// 2) Worker submits a task (issue_task_submitted)
			var signals []IssueEvent
			for _, ev := range events {
				switch ev.Type {
				case EventIssueTaskSubmitted:
					signals = append(signals, ev)
				case EventIssueTaskMessage:
					if ev.Kind == "question" || ev.Kind == "blocker" {
						signals = append(signals, ev)
					}
				}
			}
			if len(signals) > 0 {
				// Return at most one signal event.
				first := signals[0]
				advanceTo := first.Seq
				_ = s.store.WithLock(func() error {
					s.store.EnsureDir("issues", issueID, "cursors")
					return s.store.WriteJSON(cursorPath, &issueCursor{AfterSeq: advanceTo})
				})
				return []IssueEvent{first}, advanceTo, nil
			}
			// Skip non-signal events: advance cursor and keep hanging.
			afterSeq = nextSeq
			_ = s.store.WithLock(func() error {
				s.store.EnsureDir("issues", issueID, "cursors")
				return s.store.WriteJSON(cursorPath, &issueCursor{AfterSeq: afterSeq})
			})
			continue
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = s.store.WithLock(func() error {
				s.store.EnsureDir("issues", issueID, "cursors")
				return s.store.WriteJSON(cursorPath, &issueCursor{AfterSeq: afterSeq})
			})
			return []IssueEvent{}, afterSeq, nil
		}

		// Cross-process friendly long-poll: sleep a short interval then retry.
		if remaining < poll {
			time.Sleep(remaining)
		} else {
			time.Sleep(poll)
		}
	}
}
