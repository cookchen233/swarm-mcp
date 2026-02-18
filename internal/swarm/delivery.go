package swarm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (s *IssueService) CreateDelivery(actor, issueID, summary, refs string, artifacts DeliveryArtifacts) (*Delivery, error) {
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}
	if actor == "" {
		actor = "lead"
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
		result = d
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.bump("deliveries")
	return result, nil
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

func (s *IssueService) ReviewDelivery(actor, deliveryID, verdict, feedback, refs string) (*Delivery, error) {
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

// WaitDeliveries blocks until at least one delivery matching status exists.
// - If deliveries exist immediately, returns them without waiting.
// - status defaults to "open" if empty.
// - If timeoutSec <= 0, defaults to 3600.
func (s *IssueService) WaitDeliveries(status string, timeoutSec, limit int) ([]Delivery, error) {
	s.SweepExpired()
	if strings.TrimSpace(status) == "" {
		status = DeliveryOpen
	}
	timeoutSec = s.normalizeTimeoutSec(timeoutSec)
	if limit <= 0 {
		limit = 50
	}

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	poll := 200 * time.Millisecond
	for {
		s.SweepExpired()
		ds, err := s.ListDeliveries(status, "", "", "")
		if err != nil {
			return nil, err
		}
		if len(ds) > 0 {
			if len(ds) > limit {
				ds = ds[:limit]
			}
			return ds, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return []Delivery{}, nil
		}
		if remaining < poll {
			time.Sleep(remaining)
		} else {
			time.Sleep(poll)
		}
	}
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

func (s *IssueService) SubmitDelivery(actor, issueID, summary, refs string, artifacts DeliveryArtifacts, timeoutSec int) (map[string]any, error) {
	timeoutSec = s.normalizeTimeoutSec(timeoutSec)
	d, err := s.CreateDelivery(actor, issueID, summary, refs, artifacts)
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
