package swarm

import (
	"testing"
)

func TestCreateDelivery_RequiresTestEvidence(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	store.EnsureDir()
	store.EnsureDir("issues")
	store.EnsureDir("deliveries")

	trace := NewTraceService(store)
	svc := NewIssueService(store, trace, 7200, 3600, 3600)

	issueID := "issue-1"
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

	_, err := svc.CreateDelivery("lead", issueID, "sum", "", DeliveryArtifacts{
		TestResult:   "passed",
		TestCases:    []string{"go test ./..."},
		ChangedFiles: []string{"a.go"},
		ReviewedRefs: []string{"a.go"},
		TestOutput:   "ok",
	}, TestEvidence{})
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestReviewDelivery_RequiresVerificationAlignedWithEvidence(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	store.EnsureDir()
	store.EnsureDir("issues")
	store.EnsureDir("deliveries")

	trace := NewTraceService(store)
	svc := NewIssueService(store, trace, 7200, 3600, 3600)

	issueID := "issue-1"
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

	evidence := TestEvidence{
		ScriptPath:   "scripts/test-issue-1.sh",
		ScriptCmd:    "bash scripts/test-issue-1.sh",
		ScriptPassed: true,
		ScriptResult: "ok",
		DocPath:      "docs/test-issue-1.md",
		DocCommands:  []string{"echo hi"},
		DocResults: []CommandResult{
			{Command: "echo hi", Passed: true, ExitCode: 0, Output: "hi"},
		},
		DocPassed: true,
	}

	d, err := svc.CreateDelivery("lead", issueID, "sum", "", DeliveryArtifacts{
		TestResult:   "passed",
		TestCases:    []string{"go test ./..."},
		ChangedFiles: []string{"a.go"},
		ReviewedRefs: []string{"a.go"},
		TestOutput:   "ok",
	}, evidence)
	if err != nil {
		t.Fatalf("create delivery: %v", err)
	}

	_, err = svc.ClaimDelivery("acceptor", d.ID, 0)
	if err != nil {
		t.Fatalf("claim delivery: %v", err)
	}

	_, err = svc.ReviewDelivery("acceptor", d.ID, DeliveryApproved, "", "", Verification{
		ScriptPassed: true,
		ScriptResult: "ok",
		DocPassed:    true,
		DocResults:   []CommandResult{},
	})
	if err == nil {
		t.Fatalf("expected error")
	}

	out, err := svc.ReviewDelivery("acceptor", d.ID, DeliveryApproved, "", "", Verification{
		ScriptPassed: true,
		ScriptResult: "ok",
		DocPassed:    true,
		DocResults: []CommandResult{
			{Command: "echo hi", Passed: true, ExitCode: 0, Output: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("review delivery: %v", err)
	}
	if out.Status != DeliveryApproved {
		t.Fatalf("unexpected status: %s", out.Status)
	}
}
