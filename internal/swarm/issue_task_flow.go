package swarm

import (
	"fmt"
	"strings"
	"time"
)

func (s *IssueService) CreateTask(
	actor, issueID, subject, description, difficulty string,
	suggestedFiles, labels, docPaths []string,
	points int,
	contextTaskIDs []string,
	specName, splitFrom, splitReason, impactScope string, specContextTaskIDs []string,
	specGoal, specRules, specConstraints, specConventions, specAcceptance string,
) (*IssueTask, error) {
	if issueID == "" || subject == "" {
		return nil, fmt.Errorf("issue_id and subject are required")
	}
	if actor == "" {
		actor = "lead"
	}
	if difficulty != "easy" && difficulty != "medium" && difficulty != "focus" {
		return nil, fmt.Errorf("invalid difficulty: %s", difficulty)
	}
	var err error
	specName, err = cleanDocName(specName)
	if err != nil {
		return nil, fmt.Errorf("spec.name: %w", err)
	}
	splitFrom, err = trimRequired("spec_split_from", splitFrom)
	if err != nil {
		return nil, err
	}
	splitReason, err = trimRequired("spec_split_reason", splitReason)
	if err != nil {
		return nil, err
	}
	impactScope, err = trimRequired("spec_impact_scope", impactScope)
	if err != nil {
		return nil, err
	}
	// Merge context_task_ids from top-level and spec.
	ctxSeen := map[string]bool{}
	mergedCtx := make([]string, 0)
	for _, v := range append(contextTaskIDs, specContextTaskIDs...) {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if ctxSeen[v] {
			continue
		}
		ctxSeen[v] = true
		mergedCtx = append(mergedCtx, v)
	}
	specGoal, err = trimRequired("spec_goal", specGoal)
	if err != nil {
		return nil, err
	}
	specRules, err = trimRequired("spec_rules", specRules)
	if err != nil {
		return nil, err
	}
	specConstraints, err = trimRequired("spec_constraints", specConstraints)
	if err != nil {
		return nil, err
	}
	specConventions, err = trimRequired("spec_conventions", specConventions)
	if err != nil {
		return nil, err
	}
	specAcceptance, err = trimRequired("spec_acceptance", specAcceptance)
	if err != nil {
		return nil, err
	}

	var result *IssueTask
	err = s.store.WithLock(func() error {
		if !s.store.Exists("issues", issueID, "issue.json") {
			return fmt.Errorf("issue '%s' not found", issueID)
		}

		metaPath := s.store.Path("issues", issueID, "meta.json")
		var meta issueMeta
		if err := s.store.ReadJSON(metaPath, &meta); err != nil {
			return err
		}
		if meta.NextTaskNum <= 0 {
			meta.NextTaskNum = 1
		}
		taskID := fmt.Sprintf("task-%d", meta.NextTaskNum)
		meta.NextTaskNum++
		if err := s.store.WriteJSON(metaPath, &meta); err != nil {
			return err
		}

		task := &IssueTask{
			ID:                taskID,
			IssueID:           issueID,
			Subject:           subject,
			Description:       description,
			Difficulty:        difficulty,
			SplitFrom:         splitFrom,
			SplitReason:       splitReason,
			ImpactScope:       impactScope,
			ContextTaskIDs:    mergedCtx,
			SuggestedFiles:    suggestedFiles,
			Labels:            labels,
			DocPaths:          docPaths,
			RequiredIssueDocs: []string{
				// populated from issue docs below
			},
			RequiredTaskDocs: []string{specName},
			TaskDocs:         []DocRef{},
			Points:           points,
			Status:           IssueTaskOpen,
			CreatedAt:        NowStr(),
			UpdatedAt:        NowStr(),
		}

		var issue Issue
		if err := s.store.ReadJSON(s.store.Path("issues", issueID, "issue.json"), &issue); err != nil {
			return err
		}
		for _, d := range issue.Docs {
			task.RequiredIssueDocs = append(task.RequiredIssueDocs, d.Name)
			task.DocPaths = append(task.DocPaths, "issue_doc:"+d.Name)
		}

		mustRefs := []string{"task_doc:" + specName}
		seen := map[string]bool{}
		for _, p := range task.DocPaths {
			seen[p] = true
		}
		for _, r := range mustRefs {
			if !seen[r] {
				task.DocPaths = append(task.DocPaths, r)
			}
		}

		spec := strings.Join([]string{
			"# Spec",
			"",
			"## Split From",
			splitFrom,
			"",
			"## Split Reason",
			splitReason,
			"",
			"## Impact Scope",
			impactScope,
			"",
			"## Context Tasks",
			strings.Join(mergedCtx, "\n"),
			"",
			"## Goal",
			specGoal,
			"",
			"## Rules",
			specRules,
			"",
			"## Constraints",
			specConstraints,
			"",
			"## Conventions",
			specConventions,
			"",
			"## Acceptance Criteria",
			specAcceptance,
			"",
		}, "\n")
		taskDocsDir := s.store.Path("issues", issueID, "tasks", task.ID+".docs")
		specPath := s.store.Path("issues", issueID, "tasks", task.ID+".docs", specName+".md")
		if err := writeDocFile(taskDocsDir, specName+".md", spec); err != nil {
			return err
		}
		task.TaskDocs = append(task.TaskDocs, DocRef{Name: specName, Path: specPath})
		if err := s.store.WriteJSON(s.store.Path("issues", issueID, "tasks", task.ID+".json"), task); err != nil {
			return err
		}

		result = task
		return s.appendEventLocked(issueID, IssueEvent{
			Type:      EventIssueTaskCreated,
			IssueID:   issueID,
			TaskID:    task.ID,
			Actor:     actor,
			Detail:    subject,
			Timestamp: NowStr(),
		})
	})
	if err != nil {
		return nil, err
	}

	s.bump(issueID)
	return result, nil
}

func (s *IssueService) ClaimTask(issueID, taskID, actor, nextStepToken string) (*IssueTask, error) {
	if issueID == "" || taskID == "" {
		return nil, fmt.Errorf("issue_id and task_id are required")
	}
	s.SweepExpired()
	if actor == "" {
		actor = "worker"
	}
	nowMs := time.Now().UnixMilli()

	var result *IssueTask
	err := s.store.WithLock(func() error {
		task, err := s.loadTaskLocked(issueID, taskID)
		if err != nil {
			return err
		}

		if task.ReservedToken != "" {
			if task.ReservedUntilMs > 0 && nowMs > task.ReservedUntilMs {
				task.ReservedToken = ""
				task.ReservedUntilMs = 0
			} else {
				if _, err := trimRequired("next_step_token", nextStepToken); err != nil {
					return fmt.Errorf("task '%s' is reserved", taskID)
				}
				if nextStepToken != task.ReservedToken {
					return fmt.Errorf("task '%s' is reserved", taskID)
				}
				tokPath := s.store.Path("issues", issueID, "next_steps", nextStepToken+".json")
				var tok NextStepToken
				if err := s.store.ReadJSON(tokPath, &tok); err != nil {
					return fmt.Errorf("task '%s' is reserved", taskID)
				}
				if tok.IssueID != issueID || tok.Used || !tok.Attached || tok.NextStep.Type != "claim_task" || tok.NextStep.TaskID != taskID {
					return fmt.Errorf("task '%s' is reserved", taskID)
				}
				tok.Used = true
				tok.UsedAt = NowStr()
				if err := s.store.WriteJSON(tokPath, tok); err != nil {
					return err
				}
				task.ReservedToken = ""
				task.ReservedUntilMs = 0
			}
		}

		for _, n := range task.RequiredIssueDocs {
			if !s.store.Exists("issues", issueID, "docs", n+".md") {
				return fmt.Errorf("missing required issue doc: %s", n)
			}
		}
		for _, n := range task.RequiredTaskDocs {
			if !s.store.Exists("issues", issueID, "tasks", task.ID+".docs", n+".md") {
				return fmt.Errorf("missing required task doc: %s", n)
			}
		}

		if task.Status != IssueTaskOpen {
			return fmt.Errorf("task '%s' is not open (status: %s)", taskID, task.Status)
		}
		task.ClaimedBy = actor
		task.Status = IssueTaskInProgress
		task.LeaseExpiresAtMs = s.calcLeaseExpiryMs(0, s.taskTTLSec)
		task.UpdatedAt = NowStr()
		if err := s.store.WriteJSON(s.store.Path("issues", issueID, "tasks", task.ID+".json"), task); err != nil {
			return err
		}
		result = task
		return s.appendEventLocked(issueID, IssueEvent{Type: EventIssueTaskClaimed, IssueID: issueID, TaskID: task.ID, Actor: actor, Timestamp: NowStr()})
	})
	if err != nil {
		return nil, err
	}

	s.bump(issueID)
	return result, nil
}

func (s *IssueService) SubmitTask(issueID, taskID, actor string, artifacts SubmissionArtifacts) (*IssueTask, error) {
	if issueID == "" || taskID == "" {
		return nil, fmt.Errorf("issue_id and task_id are required")
	}
	s.SweepExpired()
	if actor == "" {
		actor = "worker"
	}
	if _, err := trimRequired("artifacts.summary", artifacts.Summary); err != nil {
		return nil, err
	}
	if len(artifacts.ChangedFiles) == 0 {
		return nil, fmt.Errorf("artifacts.changed_files is required")
	}
	if len(artifacts.TestCases) == 0 {
		return nil, fmt.Errorf("artifacts.test_cases is required")
	}
	if _, err := trimRequired("artifacts.test_result", artifacts.TestResult); err != nil {
		return nil, err
	}
	if _, err := trimRequired("artifacts.test_output", artifacts.TestOutput); err != nil {
		return nil, err
	}

	// waitReviewTimeoutSec will be set dynamically based on service configuration
	var submitSeq int64
	err := s.store.WithLock(func() error {
		task, err := s.loadTaskLocked(issueID, taskID)
		if err != nil {
			return err
		}
		if task.Status != IssueTaskInProgress && task.Status != IssueTaskSubmitted {
			return fmt.Errorf("task '%s' is not in progress/submitted (status: %s)", taskID, task.Status)
		}

		task.Submitter = actor
		task.SubmissionArtifacts = artifacts
		task.Status = IssueTaskSubmitted
		nowMs := time.Now().UnixMilli()
		minLeaseMs := nowMs + int64(s.defaultTimeoutSec)*1000
		if task.LeaseExpiresAtMs < minLeaseMs {
			task.LeaseExpiresAtMs = minLeaseMs
		}
		task.UpdatedAt = NowStr()
		if err := s.store.WriteJSON(s.store.Path("issues", issueID, "tasks", task.ID+".json"), task); err != nil {
			return err
		}
		ev := IssueEvent{Type: EventIssueTaskSubmitted, IssueID: issueID, TaskID: task.ID, Actor: actor, Detail: "submitted", Refs: "", SubmissionArtifacts: &artifacts, Timestamp: NowStr()}
		seq, err := s.appendEventLockedWithSeq(issueID, &ev)
		if err != nil {
			return err
		}
		submitSeq = seq
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.bump(issueID)
	deadline := time.Now().Add(time.Duration(s.defaultTimeoutSec) * time.Second)
	after := submitSeq
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("timeout waiting for lead review")
		}
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
			if ev.Type == EventIssueTaskReviewed || ev.Type == EventIssueTaskResolved {
				return s.GetTask(issueID, taskID)
			}
		}
		after = nextSeq
	}
}

func (s *IssueService) ReviewTask(actor, issueID, taskID, verdict, feedback string, completionScore int, artifacts ReviewArtifacts, feedbackDetails []FeedbackDetail, nextStepToken string) (*IssueTask, error) {
	if issueID == "" || taskID == "" {
		return nil, fmt.Errorf("issue_id and task_id are required")
	}
	if verdict != VerdictApproved && verdict != VerdictRejected {
		return nil, fmt.Errorf("invalid verdict: %s", verdict)
	}
	if completionScore != 1 && completionScore != 2 && completionScore != 5 {
		return nil, fmt.Errorf("invalid completion_score: %d", completionScore)
	}
	if _, err := trimRequired("artifacts.review_summary", artifacts.ReviewSummary); err != nil {
		return nil, err
	}
	if len(artifacts.ReviewedRefs) == 0 || len(feedbackDetails) == 0 {
		return nil, fmt.Errorf("artifacts.reviewed_refs and feedback_details are required")
	}
	for i, fd := range feedbackDetails {
		if _, err := trimRequired(fmt.Sprintf("feedback_details[%d].dimension", i), fd.Dimension); err != nil {
			return nil, err
		}
		if _, err := trimRequired(fmt.Sprintf("feedback_details[%d].severity", i), fd.Severity); err != nil {
			return nil, err
		}
		if _, err := trimRequired(fmt.Sprintf("feedback_details[%d].content", i), fd.Content); err != nil {
			return nil, err
		}
	}
	if _, err := trimRequired("next_step_token", nextStepToken); err != nil {
		return nil, err
	}
	if actor == "" {
		actor = "lead"
	}

	var result *IssueTask
	err := s.store.WithLock(func() error {
		tokPath := s.store.Path("issues", issueID, "next_steps", nextStepToken+".json")
		var tok NextStepToken
		if err := s.store.ReadJSON(tokPath, &tok); err != nil {
			return fmt.Errorf("invalid next_step_token")
		}
		if tok.IssueID != issueID || tok.Actor != actor || tok.Used {
			return fmt.Errorf("invalid next_step_token")
		}
		if tok.NextStep.Type == "claim_task" {
			t, err := s.loadTaskLocked(issueID, tok.NextStep.TaskID)
			if err != nil {
				return err
			}
			nowMs := time.Now().UnixMilli()
			if t.Status != IssueTaskOpen || t.ReservedToken != tok.Token || (t.ReservedUntilMs > 0 && nowMs > t.ReservedUntilMs) {
				return fmt.Errorf("next_step task '%s' is not reserved", tok.NextStep.TaskID)
			}
		}

		task, err := s.loadTaskLocked(issueID, taskID)
		if err != nil {
			return err
		}
		if task.Status != IssueTaskSubmitted {
			return fmt.Errorf("task '%s' is not submitted (status: %s)", taskID, task.Status)
		}

		task.Verdict = verdict
		task.Feedback = feedback
		task.CompletionScore = completionScore
		task.ReviewArtifacts = artifacts
		task.FeedbackDetails = feedbackDetails
		task.NextStepToken = nextStepToken
		if verdict == VerdictApproved {
			task.Status = IssueTaskDone
		} else {
			task.Status = IssueTaskInProgress
		}
		task.UpdatedAt = NowStr()
		if err := s.store.WriteJSON(s.store.Path("issues", issueID, "tasks", task.ID+".json"), task); err != nil {
			return err
		}
		result = task

		tok.Attached = true
		tok.AttachedAt = NowStr()
		if err := s.store.WriteJSON(tokPath, tok); err != nil {
			return err
		}

		eventType := EventIssueTaskReviewed
		if verdict == VerdictApproved {
			eventType = EventIssueTaskResolved
		}
		return s.appendEventLocked(issueID, IssueEvent{Type: eventType, IssueID: issueID, TaskID: task.ID, Actor: actor, Detail: verdict, Refs: "", ReviewArtifacts: &artifacts, FeedbackDetails: feedbackDetails, CompletionScore: completionScore, NextStep: &tok.NextStep, NextStepToken: nextStepToken, Timestamp: NowStr()})
	})
	if err != nil {
		return nil, err
	}

	s.bump(issueID)
	return result, nil
}

func (s *IssueService) GetTask(issueID, taskID string) (*IssueTask, error) {
	if issueID == "" || taskID == "" {
		return nil, fmt.Errorf("issue_id and task_id are required")
	}
	s.SweepExpired()

	var result *IssueTask
	err := s.store.WithLock(func() error {
		t, err := s.loadTaskLocked(issueID, taskID)
		if err != nil {
			return err
		}
		result = t
		return nil
	})
	return result, err
}

func (s *IssueService) ListTasks(issueID, status string) ([]IssueTask, error) {
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}
	s.SweepExpired()

	dir := s.store.Path("issues", issueID, "tasks")
	files, err := s.store.ListJSONFiles(dir)
	if err != nil {
		return nil, err
	}

	var tasks []IssueTask
	for _, f := range files {
		var t IssueTask
		if err := s.store.ReadJSON(f, &t); err != nil {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (s *IssueService) CountTasks(issueID string) (int, error) {
	if issueID == "" {
		return 0, fmt.Errorf("issue_id is required")
	}
	if !s.store.Exists("issues", issueID, "issue.json") {
		return 0, fmt.Errorf("issue '%s' not found", issueID)
	}
	dir := s.store.Path("issues", issueID, "tasks")
	files, err := s.store.ListJSONFiles(dir)
	if err != nil {
		return 0, err
	}
	return len(files), nil
}

// WaitIssueTasks blocks until at least one task matching status exists under an issue.
// - If tasks exist immediately, returns them without waiting.
// - status defaults to "open" if empty.
// - If timeoutSec <= 0, defaults to 3600.
func (s *IssueService) WaitIssueTasks(issueID, status string, timeoutSec, limit int) ([]IssueTask, error) {
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}
	s.SweepExpired()
	if strings.TrimSpace(status) == "" {
		status = IssueTaskOpen
	}
	timeoutSec = s.normalizeTimeoutSec(timeoutSec)
	if limit <= 0 {
		limit = 50
	}

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	poll := 200 * time.Millisecond
	for {
		s.SweepExpired()
		tasks, err := s.ListTasks(issueID, status)
		if err != nil {
			return nil, err
		}
		if len(tasks) > 0 {
			if len(tasks) > limit {
				tasks = tasks[:limit]
			}
			return tasks, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return []IssueTask{}, nil
		}
		if remaining < poll {
			time.Sleep(remaining)
		} else {
			time.Sleep(poll)
		}
	}
}
