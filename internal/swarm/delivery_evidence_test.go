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
	svc := NewIssueService(store, trace, 7200, 3600, 3600, 3600)

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

func TestCreateDelivery_InvalidDocPathFormat(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	store.EnsureDir()
	store.EnsureDir("issues")
	store.EnsureDir("deliveries")

	trace := NewTraceService(store)
	svc := NewIssueService(store, trace, 7200, 3600, 3600, 3600)

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

	// Test invalid doc_path formats
	invalidPaths := []string{
		"docs/test-issue-1.md",             // wrong prefix
		"docs/issue-1-test.md",             // missing "steps"
		"docs/issue-1-test-steps.txt",      // wrong extension
		"docs/issue-1-test-steps",          // missing extension
		"docs/issue-1-test-steps.md",       // valid (should pass)
		"docs/issue-123-test-steps.md",     // valid (should pass)
		"docs/issue-abc-test-steps.md",     // valid (should pass)
		"docs/issue-1-2-test-steps.md",     // valid (should pass)
		"docs/issue-1_test-steps.md",       // invalid: underscore not allowed
		"docs/issue-1-test-steps-extra.md", // invalid: extra suffix
	}

	for i, docPath := range invalidPaths {
		evidence := TestEvidence{
			ScriptPath:   "scripts/test-issue-1.sh",
			ScriptCmd:    "bash scripts/test-issue-1.sh",
			ScriptPassed: true,
			ScriptResult: "ok",
			DocPath:      docPath,
			DocCommands:  []string{"echo hi"},
			DocResults: []CommandResult{
				{Command: "echo hi", Passed: true, ExitCode: 0, Output: "hi"},
			},
			DocPassed: true,
		}

		_, err := svc.CreateDelivery("lead", issueID, "sum", "", DeliveryArtifacts{
			TestResult:   "passed",
			TestCases:    []string{"go test ./..."},
			ChangedFiles: []string{"a.go"},
			ReviewedRefs: []string{"a.go"},
			TestOutput:   "ok",
		}, evidence)

		// Only the valid ones should succeed
		isValid := (docPath == "docs/issue-1-test-steps.md" ||
			docPath == "docs/issue-123-test-steps.md" ||
			docPath == "docs/issue-abc-test-steps.md" ||
			docPath == "docs/issue-1-2-test-steps.md")

		if isValid && err != nil {
			t.Errorf("Test %d: expected valid doc_path %s to pass, but got error: %v", i, docPath, err)
		} else if !isValid && err == nil {
			t.Errorf("Test %d: expected invalid doc_path %s to fail, but it passed", i, docPath)
		}
	}
}

func TestReviewDelivery_RequiresVerificationAlignedWithEvidence(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	store.EnsureDir()
	store.EnsureDir("issues")
	store.EnsureDir("deliveries")

	trace := NewTraceService(store)
	svc := NewIssueService(store, trace, 7200, 3600, 3600, 3600)

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
		DocPath:      "docs/issue-1-test-steps.md",
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
