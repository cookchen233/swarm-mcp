package swarm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResetTask_ClearsAllProgressAndCleansLocksAndDocs(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	store.EnsureDir()
	store.EnsureDir("issues")
	store.EnsureDir("locks", "files")
	store.EnsureDir("locks", "leases")

	trace := NewTraceService(store)
	svc := NewIssueService(store, trace, 7200, 3600, 3600)

	issueID := "issue-1"
	taskID := "task-1"
	owner := "worker-1"

	store.EnsureDir("issues", issueID)
	if err := store.WriteJSON(store.Path("issues", issueID, "issue.json"), &Issue{
		ID:        issueID,
		Subject:   "s",
		Status:    IssueOpen,
		CreatedAt: NowStr(),
		UpdatedAt: NowStr(),
	}); err != nil {
		t.Fatalf("write issue: %v", err)
	}
	if err := store.WriteJSON(store.Path("issues", issueID, "meta.json"), &issueMeta{NextSeq: 1, NextTaskNum: 1}); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	// Create a task in a non-open state with artifacts and reserved token
	task := &IssueTask{
		ID:               taskID,
		IssueID:          issueID,
		Subject:          "subj",
		Description:      "desc",
		Difficulty:       "easy",
		SplitFrom:        "x",
		SplitReason:      "y",
		ImpactScope:      "z",
		RequiredTaskDocs: []string{"spec"},
		Status:           IssueTaskSubmitted,
		ReservedToken:    "ns_1",
		ReservedUntilMs:  123,
		LeaseExpiresAtMs: 999,
		ClaimedBy:        owner,
		Submitter:        owner,
		Submission:       "something",
		Refs:             "refs",
		SubmissionArtifacts: SubmissionArtifacts{
			Summary:      "sum",
			ChangedFiles: []string{"a.go"},
			TestCases:    []string{"go test"},
			TestResult:   "passed",
			TestOutput:   "ok",
		},
		Verdict:         VerdictApproved,
		Feedback:        "fb",
		CompletionScore: 5,
		ReviewArtifacts: ReviewArtifacts{ReviewSummary: "r", ReviewedRefs: []string{"a.go"}},
		FeedbackDetails: []FeedbackDetail{{Dimension: "correctness", Severity: "minor", Content: "c"}},
		NextStepToken:   "ns_tok",
		CreatedAt:       NowStr(),
		UpdatedAt:       NowStr(),
	}

	store.EnsureDir("issues", issueID, "tasks")
	if err := store.WriteJSON(store.Path("issues", issueID, "tasks", taskID+".json"), task); err != nil {
		t.Fatalf("write task: %v", err)
	}

	// Create reserved next_step token file
	store.EnsureDir("issues", issueID, "next_steps")
	if err := store.WriteJSON(store.Path("issues", issueID, "next_steps", "ns_1.json"), &NextStepToken{
		Token:     "ns_1",
		IssueID:   issueID,
		Actor:     "lead",
		NextStep:  NextStep{Type: "claim_task", TaskID: taskID},
		Attached:  true,
		Used:      false,
		CreatedAt: NowStr(),
	}); err != nil {
		t.Fatalf("write next step token: %v", err)
	}

	// Create task docs: required spec.md and an extra worker doc extra.md
	docsDir := store.Path("issues", issueID, "tasks", taskID+".docs")
	if err := os.MkdirAll(docsDir, 0755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "spec.md"), []byte("spec"), 0644); err != nil {
		t.Fatalf("write spec doc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "extra.md"), []byte("extra"), 0644); err != nil {
		t.Fatalf("write extra doc: %v", err)
	}

	// Create a file lock lease for this task
	leaseID := "l_1"
	lockedFile := "a.go"
	if err := store.WriteJSON(store.Path("locks", "leases", leaseID+".json"), &Lease{
		LeaseID:    leaseID,
		Owner:      owner,
		TaskID:     taskID,
		Files:      []string{lockedFile},
		AcquiredAt: NowStr(),
		ExpiresAt:  NowStr(),
	}); err != nil {
		t.Fatalf("write lease: %v", err)
	}
	lockHash := PathHash(lockedFile)
	if err := store.WriteJSON(store.Path("locks", "files", lockHash+".json"), &FileLock{
		LeaseID:    leaseID,
		Owner:      owner,
		TaskID:     taskID,
		File:       lockedFile,
		AcquiredAt: NowStr(),
		ExpiresAt:  NowStr(),
	}); err != nil {
		t.Fatalf("write file lock: %v", err)
	}

	out, err := svc.ResetTask("lead", issueID, taskID, "because")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}

	if out.Status != IssueTaskOpen {
		t.Fatalf("expected open, got %s", out.Status)
	}
	if out.ClaimedBy != "" || out.Submitter != "" || out.Submission != "" || out.Refs != "" {
		t.Fatalf("expected cleared identity fields")
	}
	if out.LeaseExpiresAtMs != 0 {
		t.Fatalf("expected cleared lease")
	}
	if out.ReservedToken != "" || out.ReservedUntilMs != 0 || out.NextStepToken != "" {
		t.Fatalf("expected cleared reservation")
	}
	if out.Verdict != "" || out.Feedback != "" || out.CompletionScore != 0 {
		t.Fatalf("expected cleared review state")
	}
	if out.SubmissionArtifacts.Summary != "" || len(out.SubmissionArtifacts.ChangedFiles) != 0 {
		t.Fatalf("expected cleared submission artifacts")
	}
	if out.FeedbackDetails != nil {
		t.Fatalf("expected cleared feedback details")
	}

	// Required doc exists; extra doc removed
	if _, err := os.Stat(filepath.Join(docsDir, "spec.md")); err != nil {
		t.Fatalf("expected spec.md to remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(docsDir, "extra.md")); err == nil {
		t.Fatalf("expected extra.md removed")
	}

	// Reserved token file removed
	if store.Exists("issues", issueID, "next_steps", "ns_1.json") {
		t.Fatalf("expected next step token removed")
	}

	// Locks removed
	if store.Exists("locks", "leases", leaseID+".json") {
		t.Fatalf("expected lease removed")
	}
	if store.Exists("locks", "files", lockHash+".json") {
		t.Fatalf("expected file lock removed")
	}
}
