package swarm

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func (s *IssueService) CreateIssue(actor, subject, description string, sharedDocPaths, projectDocPaths []string, userName, userContent, leadName, leadContent string, otherDocs []map[string]any) (*Issue, error) {
	if subject == "" {
		return nil, fmt.Errorf("subject is required")
	}
	if actor == "" {
		actor = "lead"
	}
	var err error
	userName, err = cleanDocName(userName)
	if err != nil {
		return nil, fmt.Errorf("user_issue_doc.name: %w", err)
	}
	userContent, err = trimRequired("user_issue_doc.content", userContent)
	if err != nil {
		return nil, err
	}
	leadName, err = cleanDocName(leadName)
	if err != nil {
		return nil, fmt.Errorf("lead_issue_doc.name: %w", err)
	}
	leadContent, err = trimRequired("lead_issue_doc.content", leadContent)
	if err != nil {
		return nil, err
	}

	issue := &Issue{
		ID:               GenID("issue"),
		Subject:          subject,
		Description:      description,
		SharedDocPaths:   sharedDocPaths,
		ProjectDocPaths:  projectDocPaths,
		Docs:             nil,
		Status:           IssueOpen,
		LeaseExpiresAtMs: s.calcLeaseExpiryMs(0, s.issueTTLSec),
		CreatedAt:        NowStr(),
		UpdatedAt:        NowStr(),
	}

	err = s.store.WithLock(func() error {
		// Persist issue
		s.store.EnsureDir("issues", issue.ID, "tasks")
		s.store.EnsureDir("issues", issue.ID, "docs")

		// Mandatory issue docs (named)
		docsDir := s.store.Path("issues", issue.ID, "docs")
		userPath := s.store.Path("issues", issue.ID, "docs", userName+".md")
		if err := writeDocFile(docsDir, userName+".md", userContent); err != nil {
			return err
		}
		leadPath := s.store.Path("issues", issue.ID, "docs", leadName+".md")
		if err := writeDocFile(docsDir, leadName+".md", leadContent); err != nil {
			return err
		}
		issue.Docs = append(issue.Docs,
			DocRef{Name: userName, Path: userPath},
			DocRef{Name: leadName, Path: leadPath},
		)
		for _, d := range otherDocs {
			n, _ := d["name"].(string)
			c, _ := d["content"].(string)
			n, err = cleanDocName(n)
			if err != nil {
				return fmt.Errorf("user_other_docs.name: %w", err)
			}
			c = strings.TrimSpace(c)
			p := s.store.Path("issues", issue.ID, "docs", n+".md")
			if err := writeDocFile(docsDir, n+".md", c); err != nil {
				return err
			}
			issue.Docs = append(issue.Docs, DocRef{Name: n, Path: p})
		}

		if err := s.store.WriteJSON(s.store.Path("issues", issue.ID, "issue.json"), issue); err != nil {
			return err
		}
		// Init meta
		meta := &issueMeta{NextSeq: 1, NextTaskNum: 1}
		if err := s.store.WriteJSON(s.store.Path("issues", issue.ID, "meta.json"), meta); err != nil {
			return err
		}
		// Append event
		return s.appendEventLocked(issue.ID, IssueEvent{
			Type:      EventIssueCreated,
			IssueID:   issue.ID,
			Actor:     actor,
			Detail:    subject,
			Timestamp: NowStr(),
		})
	})
	if err != nil {
		return nil, err
	}

	s.bump(issue.ID)
	return issue, nil
}

func (s *IssueService) UpdateIssueDocPaths(actor, issueID string, sharedDocPaths, projectDocPaths []string) (*Issue, error) {
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}
	if actor == "" {
		actor = "lead"
	}

	var result *Issue
	err := s.store.WithLock(func() error {
		var issue Issue
		if err := s.store.ReadJSON(s.store.Path("issues", issueID, "issue.json"), &issue); err != nil {
			return err
		}
		if sharedDocPaths != nil {
			issue.SharedDocPaths = sharedDocPaths
		}
		if projectDocPaths != nil {
			issue.ProjectDocPaths = projectDocPaths
		}
		issue.UpdatedAt = NowStr()
		if err := s.store.WriteJSON(s.store.Path("issues", issueID, "issue.json"), &issue); err != nil {
			return err
		}
		result = &issue
		return s.appendEventLocked(issueID, IssueEvent{
			Type:      "issue_updated",
			IssueID:   issueID,
			Actor:     actor,
			Detail:    "doc_paths_updated",
			Timestamp: NowStr(),
		})
	})
	if err != nil {
		return nil, err
	}
	s.bump(issueID)
	return result, nil
}

func (s *IssueService) ReopenIssue(actor, issueID, summary string) (*Issue, error) {
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}
	s.SweepExpired()
	if actor == "" {
		actor = "lead"
	}

	var result *Issue
	err := s.store.WithLock(func() error {
		var issue Issue
		if err := s.store.ReadJSON(s.store.Path("issues", issueID, "issue.json"), &issue); err != nil {
			return err
		}
		if issue.Status != IssueDone && issue.Status != IssueCanceled {
			return fmt.Errorf("cannot reopen issue: status must be done/canceled (status: %s)", issue.Status)
		}
		issue.Status = IssueOpen
		issue.LeaseExpiresAtMs = s.calcLeaseExpiryMs(0, s.issueTTLSec)
		issue.UpdatedAt = NowStr()
		if err := s.store.WriteJSON(s.store.Path("issues", issueID, "issue.json"), &issue); err != nil {
			return err
		}
		result = &issue
		return s.appendEventLocked(issueID, IssueEvent{
			Type:      EventIssueReopened,
			IssueID:   issueID,
			Actor:     actor,
			Detail:    summary,
			Timestamp: NowStr(),
		})
	})
	if err != nil {
		return nil, err
	}
	s.bump(issueID)
	return result, nil
}

func (s *IssueService) DeliverIssue(actor, issueID, summary, refs string, artifacts DeliveryArtifacts) (*IssueEvent, error) {
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}
	s.SweepExpired()
	if actor == "" {
		actor = "lead"
	}
	if _, err := trimRequired("summary", summary); err != nil {
		return nil, err
	}
	if artifacts.TestResult != "passed" && artifacts.TestResult != "failed" {
		return nil, fmt.Errorf("invalid artifacts.test_result: %s", artifacts.TestResult)
	}
	if len(artifacts.TestCases) == 0 {
		return nil, fmt.Errorf("artifacts.test_cases is required")
	}
	if len(artifacts.ReviewedRefs) == 0 {
		return nil, fmt.Errorf("artifacts.reviewed_refs is required")
	}

	// Validate all tasks are done before delivery.
	tasks, err := s.ListTasks(issueID, "")
	if err != nil {
		return nil, err
	}
	var notDone []string
	for _, t := range tasks {
		if t.Status != IssueTaskDone {
			notDone = append(notDone, t.ID+":"+t.Status)
		}
	}
	if len(notDone) > 0 {
		return nil, fmt.Errorf("cannot deliver issue: tasks not done: %s", strings.Join(notDone, ", "))
	}

	changedUnion := map[string]struct{}{}
	for _, t := range tasks {
		for _, f := range t.SubmissionArtifacts.ChangedFiles {
			if strings.TrimSpace(f) == "" {
				continue
			}
			changedUnion[f] = struct{}{}
		}
	}
	if len(artifacts.ChangedFiles) < len(changedUnion) {
		return nil, fmt.Errorf("artifacts.changed_files is insufficient; please review and include all changed files")
	}

	var ev *IssueEvent
	err = s.store.WithLock(func() error {
		if !s.store.Exists("issues", issueID, "issue.json") {
			return fmt.Errorf("issue '%s' not found", issueID)
		}
		art := artifacts
		e := IssueEvent{
			Type:              EventIssueDelivered,
			IssueID:           issueID,
			Actor:             actor,
			Detail:            summary,
			Refs:              refs,
			DeliveryArtifacts: &art,
			Timestamp:         NowStr(),
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

func (s *IssueService) DeliverIssueAndWaitAcceptance(actor, issueID, summary, refs string, artifacts DeliveryArtifacts, timeoutSec int) (map[string]any, error) {
	timeoutSec = s.normalizeTimeoutSec(timeoutSec)
	ev, err := s.DeliverIssue(actor, issueID, summary, refs, artifacts)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	after := ev.Seq
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("timeout waiting for acceptance")
		}
		slice := 5
		if remaining < 5*time.Second {
			slice = int(remaining.Seconds())
			if slice <= 0 {
				slice = 1
			}
		}
		events, nextSeq, err := s.WaitIssueAcceptanceEvents(issueID, actor, ev.Seq, after, slice, 50)
		if err != nil {
			return nil, err
		}
		for _, a := range events {
			return map[string]any{
				"delivery":   ev,
				"acceptance": a,
				"next_seq":   nextSeq,
				"server_now": NowStr(),
			}, nil
		}
		after = nextSeq
	}
}

// WaitIssueAcceptanceEvents blocks until there are new acceptance events for a specific delivery.
// It returns acceptance events matching parentSeq (delivery seq).
func (s *IssueService) WaitIssueAcceptanceEvents(issueID, actor string, parentSeq, afterSeq int64, timeoutSec, limit int) ([]IssueEvent, int64, error) {
	if issueID == "" {
		return nil, afterSeq, fmt.Errorf("issue_id is required")
	}
	if parentSeq <= 0 {
		return nil, afterSeq, fmt.Errorf("parent_seq is required")
	}
	s.SweepExpired()
	timeoutSec = s.normalizeTimeoutSec(timeoutSec)
	if limit <= 0 {
		limit = 20
	}

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	poll := 200 * time.Millisecond
	for {
		// Do not SweepExpired inside the polling loop to avoid excessive global lock contention
		// while a lead is blocking on delivery and an acceptor needs to write acceptance.
		events, nextSeq, err := s.readEventsAfter(issueID, afterSeq, limit)
		if err != nil {
			return nil, afterSeq, err
		}
		if len(events) > 0 {
			out := make([]IssueEvent, 0, 1)
			for _, ev := range events {
				if ev.Type == EventIssueAcceptance && ev.ParentSeq == parentSeq {
					out = append(out, ev)
				}
			}
			if len(out) > 0 {
				return out[:1], nextSeq, nil
			}
			afterSeq = nextSeq
			continue
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

// ReviewIssueDelivery is used by the acceptor role to provide final acceptance feedback for a delivery.
// - verdict=approved: returns immediately after recording acceptance event.
// - verdict=rejected: records acceptance event, then blocks until a new issue_delivered event appears after the parentSeq.
func (s *IssueService) ReviewIssueDelivery(actor, issueID string, parentSeq int64, verdict, feedback, refs string, timeoutSec int) (map[string]any, error) {
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}
	if parentSeq <= 0 {
		return nil, fmt.Errorf("parent_seq is required")
	}
	if verdict != VerdictApproved && verdict != VerdictRejected {
		return nil, fmt.Errorf("invalid verdict: %s", verdict)
	}
	s.SweepExpired()
	if actor == "" {
		actor = "acceptor"
	}
	timeoutSec = s.normalizeTimeoutSec(timeoutSec)

	var acc *IssueEvent
	err := s.store.WithLock(func() error {
		if !s.store.Exists("issues", issueID, "issue.json") {
			return fmt.Errorf("issue '%s' not found", issueID)
		}
		e := IssueEvent{
			Type:      EventIssueAcceptance,
			ParentSeq: parentSeq,
			IssueID:   issueID,
			Actor:     actor,
			Detail:    verdict,
			Kind:      "acceptance",
			Refs:      refs,
			Timestamp: NowStr(),
		}
		if strings.TrimSpace(feedback) != "" {
			e.Refs = strings.TrimSpace(strings.TrimSpace(e.Refs) + "\n" + strings.TrimSpace(feedback))
		}
		seq, err := s.appendEventLockedWithSeq(issueID, &e)
		if err != nil {
			return err
		}
		e.Seq = seq
		acc = &e
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.bump(issueID)

	if verdict == VerdictApproved {
		return map[string]any{"acceptance": acc}, nil
	}

	// Rejected: block until next delivery event
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	after := parentSeq
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("timeout waiting for re-delivery")
		}
		slice := 5
		if remaining < 5*time.Second {
			slice = int(remaining.Seconds())
			if slice <= 0 {
				slice = 1
			}
		}
		evs, nextSeq, err := s.WaitIssueDeliveryEvents(issueID, actor, after, slice, 50)
		if err != nil {
			return nil, err
		}
		for _, d := range evs {
			return map[string]any{"acceptance": acc, "next_delivery": d, "next_seq": nextSeq}, nil
		}
		after = nextSeq
	}
}

// WaitIssueDeliveryEvents blocks until there are new delivery signal events for this issue.
// Signals are:
// 1) issue delivered events (issue_delivered)
//
// Cursor semantics are the same as WaitIssueTaskEvents, but persisted separately under:
// issues/{issue_id}/cursors_acceptance/{actor}.json
func (s *IssueService) WaitIssueDeliveryEvents(issueID, actor string, afterSeq int64, timeoutSec, limit int) ([]IssueEvent, int64, error) {
	if issueID == "" {
		return nil, afterSeq, fmt.Errorf("issue_id is required")
	}
	s.SweepExpired()
	if actor == "" {
		actor = "acceptor"
	}
	timeoutSec = s.normalizeTimeoutSec(timeoutSec)
	if limit <= 0 {
		limit = 20
	}

	cursorPath := s.store.Path("issues", issueID, "cursors_acceptance", actor+".json")
	if afterSeq < 0 {
		var c issueCursor
		if err := s.store.ReadJSON(cursorPath, &c); err == nil {
			afterSeq = c.AfterSeq
		} else {
			afterSeq = -1
		}
	}

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	poll := 200 * time.Millisecond
	for {
		s.SweepExpired()
		events, nextSeq, err := s.readEventsAfter(issueID, afterSeq, limit)
		if err != nil {
			return nil, afterSeq, err
		}
		if len(events) > 0 {
			var signals []IssueEvent
			for _, ev := range events {
				if ev.Type == EventIssueDelivered {
					signals = append(signals, ev)
				}
			}
			if len(signals) > 0 {
				first := signals[0]
				advanceTo := first.Seq
				_ = s.store.WithLock(func() error {
					s.store.EnsureDir("issues", issueID, "cursors_acceptance")
					return s.store.WriteJSON(cursorPath, &issueCursor{AfterSeq: advanceTo})
				})
				return []IssueEvent{first}, advanceTo, nil
			}
			afterSeq = nextSeq
			_ = s.store.WithLock(func() error {
				s.store.EnsureDir("issues", issueID, "cursors_acceptance")
				return s.store.WriteJSON(cursorPath, &issueCursor{AfterSeq: afterSeq})
			})
			continue
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = s.store.WithLock(func() error {
				s.store.EnsureDir("issues", issueID, "cursors_acceptance")
				return s.store.WriteJSON(cursorPath, &issueCursor{AfterSeq: afterSeq})
			})
			return []IssueEvent{}, afterSeq, nil
		}
		if remaining < poll {
			time.Sleep(remaining)
		} else {
			time.Sleep(poll)
		}
	}
}

func (s *IssueService) ListIssues() ([]Issue, error) {
	s.SweepExpired()
	dir := s.store.Path("issues")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Issue{}, nil
		}
		return nil, err
	}

	var out []Issue
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		var issue Issue
		if err := s.store.ReadJSON(s.store.Path("issues", id, "issue.json"), &issue); err != nil {
			continue
		}
		out = append(out, issue)
	}
	return out, nil
}

func filterIssuesByStatus(issues []Issue, status string) []Issue {
	if status == "" {
		return issues
	}
	out := make([]Issue, 0, len(issues))
	for _, it := range issues {
		if it.Status != status {
			continue
		}
		out = append(out, it)
	}
	return out
}

// WaitIssues blocks until at least one issue matching status exists.
// - If issues exist immediately, returns them without waiting.
// - status defaults to "open" if empty.
// - If timeoutSec <= 0, defaults to 3600.
func (s *IssueService) WaitIssues(status string, timeoutSec, limit int) ([]Issue, error) {
	s.SweepExpired()
	if strings.TrimSpace(status) == "" {
		status = IssueOpen
	}
	timeoutSec = s.normalizeTimeoutSec(timeoutSec)
	if limit <= 0 {
		limit = 50
	}

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	poll := 200 * time.Millisecond
	for {
		s.SweepExpired()
		issues, err := s.ListIssues()
		if err != nil {
			return nil, err
		}
		issues = filterIssuesByStatus(issues, status)
		if len(issues) > 0 {
			if len(issues) > limit {
				issues = issues[:limit]
			}
			return issues, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return []Issue{}, nil
		}
		if remaining < poll {
			time.Sleep(remaining)
		} else {
			time.Sleep(poll)
		}
	}
}

func (s *IssueService) GetIssue(issueID string) (*Issue, error) {
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}
	s.SweepExpired()
	var issue Issue
	if err := s.store.ReadJSON(s.store.Path("issues", issueID, "issue.json"), &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

func (s *IssueService) CloseIssue(actor, issueID, summary string) (*Issue, error) {
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}
	s.SweepExpired()
	if actor == "" {
		actor = "lead"
	}

	// Validate all tasks are done.
	tasks, err := s.ListTasks(issueID, "")
	if err != nil {
		return nil, err
	}
	var notDone []string
	for _, t := range tasks {
		if t.Status != IssueTaskDone {
			notDone = append(notDone, t.ID+":"+t.Status)
		}
	}
	if len(notDone) > 0 {
		return nil, fmt.Errorf("cannot close issue: tasks not done: %s", strings.Join(notDone, ", "))
	}

	var result *Issue
	err = s.store.WithLock(func() error {
		var issue Issue
		if err := s.store.ReadJSON(s.store.Path("issues", issueID, "issue.json"), &issue); err != nil {
			return err
		}
		issue.Status = IssueDone
		issue.UpdatedAt = NowStr()
		if err := s.store.WriteJSON(s.store.Path("issues", issueID, "issue.json"), &issue); err != nil {
			return err
		}
		result = &issue
		return s.appendEventLocked(issueID, IssueEvent{
			Type:      EventIssueClosed,
			IssueID:   issueID,
			Actor:     actor,
			Detail:    summary,
			Timestamp: NowStr(),
		})
	})
	if err != nil {
		return nil, err
	}
	s.bump(issueID)
	return result, nil
}
