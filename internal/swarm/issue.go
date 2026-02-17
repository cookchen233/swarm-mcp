package swarm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func NewIssueService(store *Store, trace *TraceService, issueTTLSec, taskTTLSec, defaultTimeoutSec int) *IssueService {
	s := &IssueService{store: store, trace: trace, versions: map[string]int64{}, issueTTLSec: issueTTLSec, taskTTLSec: taskTTLSec, defaultTimeoutSec: defaultTimeoutSec}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func trimRequired(name, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}

func cleanDocName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("doc name is required")
	}
	name = filepath.Clean(name)
	name = strings.TrimPrefix(name, "/")
	name = strings.TrimSuffix(name, ".md")
	if name == "." || name == ".." {
		return "", fmt.Errorf("invalid doc name")
	}
	return name, nil
}

func writeDocFile(dir, filename, content string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, filename), []byte(content), 0644)
}

func (s *IssueService) bump(issueID string) {
	s.mu.Lock()
	s.versions[issueID]++
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *IssueService) calcLeaseExpiryMs(extendSec int, defaultSec int) int64 {
	sec := extendSec
	if sec <= 0 {
		sec = defaultSec
	}
	if sec <= 0 {
		return 0
	}
	return time.Now().UnixMilli() + int64(sec)*1000
}

func (s *IssueService) normalizeTimeoutSec(timeoutSec int) int {
	if timeoutSec <= 0 {
		return s.defaultTimeoutSec
	}
	if timeoutSec < s.defaultTimeoutSec {
		return s.defaultTimeoutSec
	}
	return timeoutSec
}

func (s *IssueService) ExtendIssueLease(actor, issueID string, extendSec int) (*Issue, error) {
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
		if issue.Status != IssueOpen && issue.Status != IssueInProgress {
			return fmt.Errorf("issue '%s' is not open/in_progress (status: %s)", issueID, issue.Status)
		}
		issue.LeaseExpiresAtMs = s.calcLeaseExpiryMs(extendSec, s.issueTTLSec)
		issue.UpdatedAt = NowStr()
		if err := s.store.WriteJSON(s.store.Path("issues", issueID, "issue.json"), &issue); err != nil {
			return err
		}
		result = &issue
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.bump(issueID)
	return result, nil
}

func (s *IssueService) ExtendIssueTaskLease(actor, issueID, taskID string, extendSec int) (*IssueTask, error) {
	if issueID == "" || taskID == "" {
		return nil, fmt.Errorf("issue_id and task_id are required")
	}
	if actor == "" {
		actor = "worker"
	}

	var result *IssueTask
	err := s.store.WithLock(func() error {
		task, err := s.loadTaskLocked(issueID, taskID)
		if err != nil {
			return err
		}
		if task.ClaimedBy != actor {
			return fmt.Errorf("task '%s' is not claimed by actor", taskID)
		}
		if task.Status != IssueTaskInProgress && task.Status != IssueTaskBlocked && task.Status != IssueTaskSubmitted {
			return fmt.Errorf("task '%s' is not in progress/blocked/submitted (status: %s)", taskID, task.Status)
		}
		task.LeaseExpiresAtMs = s.calcLeaseExpiryMs(extendSec, s.taskTTLSec)
		task.UpdatedAt = NowStr()
		if err := s.store.WriteJSON(s.store.Path("issues", issueID, "tasks", task.ID+".json"), task); err != nil {
			return err
		}
		result = task
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.bump(issueID)
	return result, nil
}

func (s *IssueService) SweepExpired() {
	nowMs := time.Now().UnixMilli()
	_ = s.store.WithLock(func() error {
		issuesDir := s.store.Path("issues")
		entries, err := os.ReadDir(issuesDir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			issueID := e.Name()
			var issue Issue
			if err := s.store.ReadJSON(s.store.Path("issues", issueID, "issue.json"), &issue); err != nil {
				continue
			}

			if (issue.Status == IssueOpen || issue.Status == IssueInProgress) && issue.LeaseExpiresAtMs > 0 && nowMs > issue.LeaseExpiresAtMs {
				issue.Status = IssueCanceled
				issue.UpdatedAt = NowStr()
				_ = s.store.WriteJSON(s.store.Path("issues", issueID, "issue.json"), &issue)
				_ = s.appendEventLocked(issueID, IssueEvent{Type: EventIssueExpired, IssueID: issueID, Actor: "system", Detail: "expired", Timestamp: NowStr()})
			}

			taskFiles, _ := s.store.ListJSONFiles(s.store.Path("issues", issueID, "tasks"))
			for _, p := range taskFiles {
				var task IssueTask
				if err := s.store.ReadJSON(p, &task); err != nil {
					continue
				}
				if (task.Status == IssueTaskInProgress || task.Status == IssueTaskBlocked || task.Status == IssueTaskSubmitted) && task.LeaseExpiresAtMs > 0 && nowMs > task.LeaseExpiresAtMs {
					prevStatus := task.Status
					prevOwner := task.ClaimedBy
					task.Status = IssueTaskOpen
					task.ClaimedBy = ""
					task.Submitter = ""
					task.Submission = ""
					task.Refs = ""
					task.SubmissionArtifacts = SubmissionArtifacts{}
					task.Verdict = ""
					task.Feedback = ""
					task.CompletionScore = 0
					task.ReviewArtifacts = ReviewArtifacts{}
					task.FeedbackDetails = nil
					task.UpdatedAt = NowStr()
					_ = s.store.WriteJSON(p, &task)
					_ = s.appendEventLocked(issueID, IssueEvent{Type: EventIssueTaskExpired, IssueID: issueID, TaskID: task.ID, Actor: "system", Detail: fmt.Sprintf("expired: %s claimed_by=%s", prevStatus, prevOwner), Timestamp: NowStr()})
				}
			}
		}

		// Sweep expired in_review deliveries
		deliveriesDir := s.store.Path("deliveries")
		deliveryFiles, _ := s.store.ListJSONFiles(deliveriesDir)
		for _, p := range deliveryFiles {
			var d Delivery
			if err := s.store.ReadJSON(p, &d); err != nil {
				continue
			}
			if d.Status == DeliveryInReview && d.LeaseExpiresAtMs > 0 && nowMs > d.LeaseExpiresAtMs {
				prevClaimedBy := d.ClaimedBy
				d.Status = DeliveryOpen
				d.ClaimedBy = ""
				d.ClaimedAt = ""
				d.LeaseExpiresAtMs = 0
				d.UpdatedAt = NowStr()
				_ = s.store.WriteJSON(p, &d)
				// Note: deliveries don't have event log, so no event append
				_ = prevClaimedBy // silence unused
			}
		}

		return nil
	})
}

func (s *IssueService) loadTaskLocked(issueID, taskID string) (*IssueTask, error) {
	path := s.store.Path("issues", issueID, "tasks", taskID+".json")
	var task IssueTask
	if err := s.store.ReadJSON(path, &task); err != nil {
		return nil, fmt.Errorf("task '%s' not found in issue '%s'", taskID, issueID)
	}
	return &task, nil
}
