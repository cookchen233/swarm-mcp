package swarm

import (
	"fmt"
	"os"
	"strings"
)

// submissionsDir returns the path to the submissions directory for a task.
func (s *IssueService) submissionsDir(issueID, taskID string) string {
	return s.store.Path("issues", issueID, "submissions", taskID)
}

// createSubmissionLocked creates a new Submission entity. Must be called under store lock.
func (s *IssueService) createSubmissionLocked(issueID, taskID, workerID string, artifacts SubmissionArtifacts) (*Submission, error) {
	sub := &Submission{
		ID:        GenID("sub"),
		IssueID:   issueID,
		TaskID:    taskID,
		WorkerID:  workerID,
		Artifacts: artifacts,
		Status:    SubmissionOpen,
		CreatedAt: NowStr(),
		UpdatedAt: NowStr(),
	}
	s.store.EnsureDir("issues", issueID, "submissions", taskID)
	path := s.store.Path("issues", issueID, "submissions", taskID, sub.ID+".json")
	if err := s.store.WriteJSON(path, sub); err != nil {
		return nil, err
	}
	return sub, nil
}

// getSubmissionLocked reads a single submission. Must be called under store lock.
func (s *IssueService) getSubmissionLocked(issueID, submissionID string) (*Submission, error) {
	// submissionID encodes taskID inside, but we search all task dirs
	var found *Submission
	tasksDir := s.store.Path("issues", issueID, "submissions")
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("submission '%s' not found", submissionID)
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := s.store.Path("issues", issueID, "submissions", e.Name(), submissionID+".json")
		var sub Submission
		if err := s.store.ReadJSON(path, &sub); err == nil {
			found = &sub
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("submission '%s' not found", submissionID)
	}
	return found, nil
}

// getLatestOpenSubmissionLocked returns the most recently created open submission for a task.
func (s *IssueService) getLatestOpenSubmissionLocked(issueID, taskID string) (*Submission, error) {
	dir := s.store.Path("issues", issueID, "submissions", taskID)
	files, err := s.store.ListJSONFiles(dir)
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("no open submission for task '%s'", taskID)
	}
	var latest *Submission
	for _, f := range files {
		var sub Submission
		if err := s.store.ReadJSON(f, &sub); err != nil {
			continue
		}
		if sub.Status != SubmissionOpen {
			continue
		}
		if latest == nil || sub.CreatedAt > latest.CreatedAt {
			latest = &sub
		}
	}
	if latest == nil {
		return nil, fmt.Errorf("no open submission for task '%s'", taskID)
	}
	return latest, nil
}

// reviewSubmissionLocked reviews a submission entity. Must be called under store lock.
func (s *IssueService) reviewSubmissionLocked(
	issueID, submissionID, actor, verdict, feedback string,
	completionScore int, artifacts ReviewArtifacts, feedbackDetails []FeedbackDetail, nextStepToken string,
) (*Submission, error) {
	sub, err := s.getSubmissionLocked(issueID, submissionID)
	if err != nil {
		return nil, err
	}
	if sub.Status != SubmissionOpen {
		return nil, fmt.Errorf("submission '%s' is already %s", submissionID, sub.Status)
	}
	if verdict == VerdictApproved {
		sub.Status = SubmissionApproved
	} else {
		sub.Status = SubmissionRejected
	}
	sub.Feedback = feedback
	sub.ReviewArtifacts = artifacts
	sub.FeedbackDetails = feedbackDetails
	sub.CompletionScore = completionScore
	sub.NextStepToken = nextStepToken
	sub.ReviewedBy = actor
	sub.UpdatedAt = NowStr()

	path := s.store.Path("issues", issueID, "submissions", sub.TaskID, sub.ID+".json")
	if err := s.store.WriteJSON(path, sub); err != nil {
		return nil, err
	}
	return sub, nil
}

// GetSubmission returns a single submission by ID.
func (s *IssueService) GetSubmission(issueID, submissionID string) (*Submission, error) {
	var result *Submission
	err := s.store.WithLock(func() error {
		sub, err := s.getSubmissionLocked(issueID, submissionID)
		if err != nil {
			return err
		}
		result = sub
		return nil
	})
	return result, err
}

// ListSubmissions returns all submissions for a task.
func (s *IssueService) ListSubmissions(issueID, taskID string) ([]Submission, error) {
	dir := s.store.Path("issues", issueID, "submissions", taskID)
	files, err := s.store.ListJSONFiles(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Submission{}, nil
		}
		return nil, err
	}
	var out []Submission
	for _, f := range files {
		var sub Submission
		if err := s.store.ReadJSON(f, &sub); err != nil {
			continue
		}
		out = append(out, sub)
	}
	return out, nil
}

// deleteSubmissionsForTaskLocked removes all submission files for a task. Call under store lock.
func (s *IssueService) deleteSubmissionsForTaskLocked(issueID, taskID string) {
	dir := s.store.Path("issues", issueID, "submissions", taskID)
	files, _ := s.store.ListJSONFiles(dir)
	for _, f := range files {
		_ = s.store.Remove(f)
	}
	_ = os.Remove(dir) // remove empty dir; ignore error if not empty
	// Also remove parent submissions dir if empty
	parent := s.store.Path("issues", issueID, "submissions")
	entries, err := os.ReadDir(parent)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(parent)
	}
}

// pollSubmissionStatus polls until the submission is no longer open. Used by SubmitTask blocking wait.
func (s *IssueService) pollSubmissionStatus(issueID, submissionID string, timeoutSec int) (*Submission, error) {
	deadline := s.deadline(timeoutSec)
	for {
		var sub *Submission
		_ = s.store.WithLock(func() error {
			found, err := s.getSubmissionLocked(issueID, submissionID)
			if err == nil {
				sub = found
			}
			return nil
		})
		if sub != nil && sub.Status != SubmissionOpen {
			return sub, nil
		}
		if timeExpired(deadline) {
			return nil, fmt.Errorf("timeout waiting for review of submission '%s'", submissionID)
		}
		sleepPoll()
	}
}

// submissionPath returns the file path for a submission.
func (s *IssueService) submissionPath(issueID, taskID, submissionID string) string {
	return s.store.Path("issues", issueID, "submissions", taskID, submissionID+".json")
}

// markAllTaskSubmissionsObsolete closes any open submissions except the given one. Call under lock.
func (s *IssueService) markAllTaskSubmissionsObsolete(issueID, taskID, exceptID string) {
	dir := s.store.Path("issues", issueID, "submissions", taskID)
	files, _ := s.store.ListJSONFiles(dir)
	for _, f := range files {
		var sub Submission
		if err := s.store.ReadJSON(f, &sub); err != nil {
			continue
		}
		if sub.ID == exceptID || sub.Status != SubmissionOpen {
			continue
		}
		sub.Status = "obsolete"
		sub.UpdatedAt = NowStr()
		_ = s.store.WriteJSON(f, &sub)
	}
}

// obsoleteTaskSubmissions marks all open submissions for a task as obsolete (used on reject).
func (s *IssueService) obsoleteTaskSubmissions(issueID, taskID string) {
	dir := s.store.Path("issues", issueID, "submissions", taskID)
	files, _ := s.store.ListJSONFiles(dir)
	for _, f := range files {
		var sub Submission
		if err := s.store.ReadJSON(f, &sub); err != nil {
			continue
		}
		if sub.Status != SubmissionOpen {
			continue
		}
		sub.Status = "obsolete"
		sub.UpdatedAt = NowStr()
		_ = s.store.WriteJSON(f, &sub)
	}
}

// isValidForReview checks whether the submission id resolves. Returns sub or nil.
func (s *IssueService) resolveSubmissionForReview(issueID, taskID, submissionID string) (*Submission, error) {
	if strings.TrimSpace(submissionID) != "" {
		return s.getSubmissionLocked(issueID, submissionID)
	}
	return s.getLatestOpenSubmissionLocked(issueID, taskID)
}
