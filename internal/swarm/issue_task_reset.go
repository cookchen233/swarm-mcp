package swarm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (s *IssueService) ResetTask(actor, issueID, taskID, reason string) (*IssueTask, error) {
	if issueID == "" || taskID == "" {
		return nil, fmt.Errorf("issue_id and task_id are required")
	}
	if actor == "" {
		actor = "lead"
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "reset"
	}

	var result *IssueTask
	err := s.store.WithLock(func() error {
		if !s.store.Exists("issues", issueID, "issue.json") {
			return fmt.Errorf("issue '%s' not found", issueID)
		}

		task, err := s.loadTaskLocked(issueID, taskID)
		if err != nil {
			return err
		}
		prevOwner := strings.TrimSpace(task.ClaimedBy)

		// 1) Clear task reservation / tokens
		if strings.TrimSpace(task.ReservedToken) != "" {
			tok := strings.TrimSpace(task.ReservedToken)
			tokPath := s.store.Path("issues", issueID, "next_steps", tok+".json")
			_ = s.store.Remove(tokPath)
		}
		if strings.TrimSpace(task.NextStepToken) != "" {
			tok := strings.TrimSpace(task.NextStepToken)
			tokPath := s.store.Path("issues", issueID, "next_steps", tok+".json")
			_ = s.store.Remove(tokPath)
		}
		task.ReservedToken = ""
		task.ReservedUntilMs = 0
		task.NextStepToken = ""

		// 2) Release any file locks (leases) tied to this task
		leasesDir := s.store.Path("locks", "leases")
		leaseFiles, _ := s.store.ListJSONFiles(leasesDir)
		for _, lf := range leaseFiles {
			var lease Lease
			if err := s.store.ReadJSON(lf, &lease); err != nil {
				continue
			}
			// Note: lock leases do not carry issue_id, so to avoid cross-issue collisions
			// (task IDs can repeat across issues), we also match by previous owner when possible.
			if lease.TaskID != taskID {
				continue
			}
			if prevOwner != "" && strings.TrimSpace(lease.Owner) != prevOwner {
				continue
			}
			for _, file := range lease.Files {
				hash := PathHash(file)
				lockPath := s.store.Path("locks", "files", hash+".json")
				var fl FileLock
				if err := s.store.ReadJSON(lockPath, &fl); err == nil && fl.LeaseID == lease.LeaseID {
					_ = s.store.Remove(lockPath)
				}
			}
			_ = s.store.Remove(lf)
		}

		// Defensive cleanup: remove any leftover file locks by TaskID (even if lease file is missing)
		locksDir := s.store.Path("locks", "files")
		lockFiles, _ := s.store.ListJSONFiles(locksDir)
		for _, fp := range lockFiles {
			var fl FileLock
			if err := s.store.ReadJSON(fp, &fl); err != nil {
				continue
			}
			if fl.TaskID == taskID {
				if prevOwner != "" && strings.TrimSpace(fl.Owner) != prevOwner {
					continue
				}
				_ = s.store.Remove(fp)
			}
		}

		// 3) Clear worker execution state - bring back to "never claimed" open state
		task.Status = IssueTaskOpen
		task.LeaseExpiresAtMs = 0
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

		// 3b) Clean up Submission entities, TaskMessages, and inbox items for this task.
		s.deleteSubmissionsForTaskLocked(issueID, taskID)
		s.deleteMessagesForTaskLocked(issueID, taskID)
		s.deleteInboxForTaskLocked(issueID, taskID)

		eventsPath := s.store.Path("issues", issueID, "events.jsonl")
		if f, err := os.Open(eventsPath); err == nil {
			tmp := eventsPath + ".tmp"
			out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
			if err == nil {
				w := bufio.NewWriter(out)
				scanner := bufio.NewScanner(f)
				buf := make([]byte, 0, 1024*1024)
				scanner.Buffer(buf, 16*1024*1024)
				for scanner.Scan() {
					line := scanner.Bytes()
					if len(line) == 0 {
						continue
					}
					var ev IssueEvent
					if err := json.Unmarshal(line, &ev); err != nil {
						continue
					}
					if ev.TaskID == taskID {
						continue
					}
					_, _ = w.Write(line)
					_, _ = w.WriteString("\n")
				}
				_ = w.Flush()
				_ = out.Close()
				if err := scanner.Err(); err == nil {
					_ = os.Rename(tmp, eventsPath)
				} else {
					_ = os.Remove(tmp)
				}
			} else {
				_ = os.Remove(tmp)
			}
			_ = f.Close()
		} else {
			if !os.IsNotExist(err) {
				return err
			}
		}

		// 4) Remove non-required task docs (keep required/spec docs created at task creation)
		// RequiredTaskDocs can include subdirectories, so we do a WalkDir and compare by relative cleaned path.
		docsDir := s.store.Path("issues", issueID, "tasks", taskID+".docs")
		keep := map[string]bool{}
		for _, n := range task.RequiredTaskDocs {
			clean := filepath.Clean(n)
			clean = strings.TrimPrefix(clean, "/")
			keep[clean] = true
		}
		_ = filepath.WalkDir(docsDir, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
				return nil
			}
			rel, err := filepath.Rel(docsDir, path)
			if err != nil {
				return nil
			}
			rel = filepath.Clean(rel)
			rel = strings.TrimSuffix(rel, ".md")
			rel = strings.TrimSuffix(rel, ".MD")
			if keep[rel] {
				return nil
			}
			_ = os.Remove(path)
			return nil
		})

		if err := s.store.WriteJSON(s.store.Path("issues", issueID, "tasks", task.ID+".json"), task); err != nil {
			return err
		}
		result = task

		return s.appendEventLocked(issueID, IssueEvent{Type: EventIssueTaskReset, IssueID: issueID, TaskID: task.ID, Actor: actor, Detail: reason, Timestamp: NowStr()})
	})
	if err != nil {
		return nil, err
	}

	s.bump(issueID)
	return result, nil
}
