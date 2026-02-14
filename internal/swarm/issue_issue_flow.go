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

// WaitIssues blocks until the issue pool count becomes > afterCount.
// - If afterCount < 0, it uses the current count as the baseline (tail semantics).
// - If timeoutSec <= 0, defaults to 600.
// Cross-process friendly long-poll: polls filesystem periodically.
func (s *IssueService) WaitIssues(afterCount, timeoutSec int) ([]Issue, int, error) {
	s.SweepExpired()
	if timeoutSec <= 0 {
		timeoutSec = 600
	}
	if afterCount < 0 {
		issues, err := s.ListIssues()
		if err != nil {
			return nil, 0, err
		}
		issues = filterIssuesByStatus(issues, IssueOpen)
		afterCount = len(issues)
	}

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	poll := 200 * time.Millisecond
	for {
		s.SweepExpired()
		issues, err := s.ListIssues()
		if err != nil {
			return nil, 0, err
		}
		issues = filterIssuesByStatus(issues, IssueOpen)
		if len(issues) > afterCount {
			return issues, len(issues), nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return issues, len(issues), nil
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
