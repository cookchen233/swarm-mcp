package swarm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func validateTestEvidence(e TestEvidence) error {
	if _, err := trimRequired("test_evidence.script_path", e.ScriptPath); err != nil {
		return err
	}
	if _, err := trimRequired("test_evidence.script_cmd", e.ScriptCmd); err != nil {
		return err
	}
	if _, err := trimRequired("test_evidence.script_result", e.ScriptResult); err != nil {
		return err
	}
	if _, err := trimRequired("test_evidence.doc_path", e.DocPath); err != nil {
		return err
	}
	if len(e.DocCommands) == 0 {
		return fmt.Errorf("test_evidence.doc_commands is required")
	}
	for i, c := range e.DocCommands {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("test_evidence.doc_commands[%d] is empty", i)
		}
	}
	if len(e.DocResults) != len(e.DocCommands) {
		return fmt.Errorf("test_evidence.doc_results must align with doc_commands")
	}
	for i, r := range e.DocResults {
		if strings.TrimSpace(r.Command) == "" {
			return fmt.Errorf("test_evidence.doc_results[%d].command is required", i)
		}
		if strings.TrimSpace(r.Output) == "" {
			return fmt.Errorf("test_evidence.doc_results[%d].output is required", i)
		}
	}
	return nil
}

func validateVerification(v Verification, e TestEvidence) error {
	if _, err := trimRequired("verification.script_result", v.ScriptResult); err != nil {
		return err
	}
	if len(e.DocCommands) == 0 {
		return fmt.Errorf("delivery test_evidence.doc_commands is empty")
	}
	if len(v.DocResults) != len(e.DocCommands) {
		return fmt.Errorf("verification.doc_results must align with delivery test_evidence.doc_commands")
	}
	for i, r := range v.DocResults {
		if strings.TrimSpace(r.Command) == "" {
			return fmt.Errorf("verification.doc_results[%d].command is required", i)
		}
		if strings.TrimSpace(r.Output) == "" {
			return fmt.Errorf("verification.doc_results[%d].output is required", i)
		}
	}
	return nil
}

func (s *IssueService) CreateDelivery(actor, issueID, summary, refs string, artifacts DeliveryArtifacts, evidence TestEvidence) (*Delivery, error) {
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}
	if actor == "" {
		actor = "lead"
	}
	if err := validateTestEvidence(evidence); err != nil {
		return nil, err
	}
	if _, err := trimRequired("summary", summary); err != nil {
		return nil, err
	}
	if artifacts.TestResult != "passed" && artifacts.TestResult != "failed" {
		return nil, fmt.Errorf("artifacts.test_result must be 'passed' or 'failed'")
	}
	if len(artifacts.TestCases) == 0 {
		return nil, fmt.Errorf("artifacts.test_cases is required")
	}
	if len(artifacts.ChangedFiles) == 0 {
		return nil, fmt.Errorf("artifacts.changed_files is required")
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

	s.SweepExpired()

	var result *Delivery
	err = s.store.WithLock(func() error {
		if !s.store.Exists("issues", issueID, "issue.json") {
			return fmt.Errorf("issue '%s' not found", issueID)
		}

		d := &Delivery{
			ID:               GenID("delivery"),
			IssueID:          issueID,
			Summary:          strings.TrimSpace(summary),
			Refs:             strings.TrimSpace(refs),
			Artifacts:        artifacts,
			TestEvidence:     evidence,
			Verification:     Verification{},
			Status:           DeliveryOpen,
			DeliveredBy:      actor,
			ClaimedBy:        "",
			ReviewedBy:       "",
			Feedback:         "",
			DeliveredAt:      NowStr(),
			ClaimedAt:        "",
			ReviewedAt:       "",
			LeaseExpiresAtMs: 0,
			UpdatedAt:        NowStr(),
		}
		if err := s.store.WriteJSON(s.store.Path("deliveries", d.ID+".json"), d); err != nil {
			return err
		}
		// Push to acceptor inbox for reliable claim-based waiting.
		if _, err := s.pushToAcceptorInboxLocked(issueID, d.ID, actor); err != nil {
			return err
		}
		result = d
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.bump("deliveries")
	return result, nil
}

// WaitDeliveries blocks until at least one open delivery is available for review.
// It uses acceptor inbox claim semantics (single-consumer). Returned deliveries are already claimed (status=in_review).
// status is kept for backward compatibility; only "open" is supported in v2.
func (s *IssueService) WaitDeliveries(status string, timeoutSec, limit int) ([]Delivery, error) {
	s.SweepExpired()
	status = strings.TrimSpace(strings.ToLower(status))
	if status == "" {
		status = DeliveryOpen
	}
	if status != DeliveryOpen {
		return nil, fmt.Errorf("only status '%s' is supported", DeliveryOpen)
	}
	timeoutSec = s.normalizeTimeoutSec(timeoutSec)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	deadline := s.deadline(timeoutSec)
	out := make([]Delivery, 0, limit)
	for len(out) < limit {
		if timeExpired(deadline) {
			break
		}
		item, err := s.claimAcceptorDeliveryInboxBlocking("acceptor", int(time.Until(deadline).Seconds()))
		if err != nil {
			return nil, err
		}
		if item == nil {
			break
		}

		// Claim the delivery (atomically transitions to in_review).
		d, err := s.ClaimDelivery("acceptor", item.RefID, 0)
		if err != nil {
			// If claim fails (already claimed/reviewed), mark inbox done to prevent reprocessing.
			_ = s.store.WithLock(func() error {
				s.ackAcceptorInboxByDeliveryLocked(item.RefID)
				return nil
			})
			continue
		}
		_ = s.store.WithLock(func() error {
			s.ackAcceptorInboxByDeliveryLocked(item.RefID)
			return nil
		})
		out = append(out, *d)
	}
	return out, nil
}

func (s *IssueService) GetDelivery(deliveryID string) (*Delivery, error) {
	if deliveryID == "" {
		return nil, fmt.Errorf("delivery_id is required")
	}
	var d Delivery
	if err := s.store.ReadJSON(s.store.Path("deliveries", deliveryID+".json"), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *IssueService) ListDeliveries(status, issueID, deliveredBy, reviewedBy string) ([]Delivery, error) {
	s.SweepExpired()

	status = strings.TrimSpace(strings.ToLower(status))
	if status == "" {
		status = "all"
	}
	issueID = strings.TrimSpace(issueID)
	deliveredBy = strings.TrimSpace(deliveredBy)
	reviewedBy = strings.TrimSpace(reviewedBy)

	dir := s.store.Path("deliveries")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Delivery{}, nil
		}
		return nil, err
	}

	var out []Delivery
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var d Delivery
		if err := s.store.ReadJSON(filepath.Join(dir, e.Name()), &d); err != nil {
			continue
		}
		if status != "all" {
			if d.Status != status {
				continue
			}
		}
		if issueID != "" && d.IssueID != issueID {
			continue
		}
		if deliveredBy != "" && d.DeliveredBy != deliveredBy {
			continue
		}
		if reviewedBy != "" && d.ReviewedBy != reviewedBy {
			continue
		}
		out = append(out, d)
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].DeliveredAt > out[j].DeliveredAt
	})
	return out, nil
}

func (s *IssueService) ClaimDelivery(actor, deliveryID string, extendSec int) (*Delivery, error) {
	if deliveryID == "" {
		return nil, fmt.Errorf("delivery_id is required")
	}
	if actor == "" {
		actor = "acceptor"
	}
	s.SweepExpired()

	var result *Delivery
	err := s.store.WithLock(func() error {
		var d Delivery
		if err := s.store.ReadJSON(s.store.Path("deliveries", deliveryID+".json"), &d); err != nil {
			return err
		}
		if d.Status != DeliveryOpen {
			return fmt.Errorf("delivery '%s' is not open (status: %s)", deliveryID, d.Status)
		}
		d.Status = DeliveryInReview
		d.ClaimedBy = actor
		d.ClaimedAt = NowStr()
		ttlSec := extendSec
		if ttlSec <= 0 {
			ttlSec = s.issueTTLSec
		}
		if ttlSec <= 0 {
			ttlSec = s.defaultTimeoutSec
		}
		if ttlSec < s.defaultTimeoutSec {
			ttlSec = s.defaultTimeoutSec
		}
		d.LeaseExpiresAtMs = time.Now().UnixMilli() + int64(ttlSec)*1000
		d.UpdatedAt = NowStr()
		if err := s.store.WriteJSON(s.store.Path("deliveries", deliveryID+".json"), &d); err != nil {
			return err
		}
		result = &d
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.bump("deliveries")
	return result, nil
}

func (s *IssueService) ExtendDeliveryLease(actor, deliveryID string, extendSec int) (*Delivery, error) {
	if deliveryID == "" {
		return nil, fmt.Errorf("delivery_id is required")
	}
	if actor == "" {
		actor = "acceptor"
	}
	s.SweepExpired()

	var result *Delivery
	err := s.store.WithLock(func() error {
		var d Delivery
		if err := s.store.ReadJSON(s.store.Path("deliveries", deliveryID+".json"), &d); err != nil {
			return err
		}
		if d.Status != DeliveryInReview {
			return fmt.Errorf("delivery '%s' is not in_review (status: %s)", deliveryID, d.Status)
		}
		if d.ClaimedBy != actor {
			return fmt.Errorf("delivery '%s' is not claimed by actor", deliveryID)
		}
		ttlSec := extendSec
		if ttlSec <= 0 {
			ttlSec = s.issueTTLSec
		}
		if ttlSec <= 0 {
			ttlSec = s.defaultTimeoutSec
		}
		if ttlSec < s.defaultTimeoutSec {
			ttlSec = s.defaultTimeoutSec
		}
		d.LeaseExpiresAtMs = time.Now().UnixMilli() + int64(ttlSec)*1000
		d.UpdatedAt = NowStr()
		if err := s.store.WriteJSON(s.store.Path("deliveries", deliveryID+".json"), &d); err != nil {
			return err
		}
		result = &d
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.bump("deliveries")
	return result, nil
}

func (s *IssueService) ReviewDelivery(actor, deliveryID, verdict, feedback, refs string, verification Verification) (*Delivery, error) {
	if deliveryID == "" {
		return nil, fmt.Errorf("delivery_id is required")
	}
	verdict = strings.TrimSpace(strings.ToLower(verdict))
	if verdict != DeliveryApproved && verdict != DeliveryRejected {
		return nil, fmt.Errorf("invalid verdict: %s", verdict)
	}
	if actor == "" {
		actor = "acceptor"
	}
	feedback = strings.TrimSpace(feedback)
	refs = strings.TrimSpace(refs)

	s.SweepExpired()

	var result *Delivery
	err := s.store.WithLock(func() error {
		var d Delivery
		if err := s.store.ReadJSON(s.store.Path("deliveries", deliveryID+".json"), &d); err != nil {
			return err
		}
		if d.Status != DeliveryInReview {
			return fmt.Errorf("delivery '%s' is not in_review (status: %s)", deliveryID, d.Status)
		}
		if d.ClaimedBy != actor {
			return fmt.Errorf("delivery '%s' is not claimed by actor", deliveryID)
		}
		if err := validateVerification(verification, d.TestEvidence); err != nil {
			return err
		}
		d.Verification = verification
		d.Status = verdict
		d.ReviewedBy = actor
		d.ReviewedAt = NowStr()
		if feedback != "" {
			d.Feedback = feedback
		}
		if refs != "" {
			d.Refs = strings.TrimSpace(strings.TrimSpace(d.Refs) + "\n" + refs)
		}
		d.UpdatedAt = NowStr()
		if err := s.store.WriteJSON(s.store.Path("deliveries", deliveryID+".json"), &d); err != nil {
			return err
		}
		result = &d
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.bump("deliveries")
	return result, nil
}

func (s *IssueService) WaitDeliveryReviewed(deliveryID string, timeoutSec int) (*Delivery, error) {
	if deliveryID == "" {
		return nil, fmt.Errorf("delivery_id is required")
	}
	timeoutSec = s.normalizeTimeoutSec(timeoutSec)

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	poll := 200 * time.Millisecond
	for {
		s.SweepExpired()
		d, err := s.GetDelivery(deliveryID)
		if err != nil {
			return nil, err
		}
		if d.Status == DeliveryApproved || d.Status == DeliveryRejected {
			return d, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("timeout waiting for delivery review")
		}
		if remaining < poll {
			time.Sleep(remaining)
		} else {
			time.Sleep(poll)
		}
	}
}

func (s *IssueService) SubmitDelivery(actor, issueID, summary, refs string, artifacts DeliveryArtifacts, evidence TestEvidence, timeoutSec int) (map[string]any, error) {
	timeoutSec = s.normalizeTimeoutSec(timeoutSec)
	d, err := s.CreateDelivery(actor, issueID, summary, refs, artifacts, evidence)
	if err != nil {
		return nil, err
	}
	reviewed, err := s.WaitDeliveryReviewed(d.ID, timeoutSec)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"delivery":    d,
		"reviewed":    reviewed,
		"server_now":  NowStr(),
		"delivery_id": d.ID,
	}, nil
}
