package swarm

import (
	"os"
	"strings"
	"time"
)

const inboxClaimTTLSec = 300 // 5 min: if lead claims but doesn't process, item resets to pending

// pushToLeadInbox adds a pending item to the issue's lead inbox. Call under store lock.
func (s *IssueService) pushToLeadInboxLocked(issueID, taskID, itemType, refID, senderID string) (*InboxItem, error) {
	item := &InboxItem{
		ID:        GenID("inb"),
		IssueID:   issueID,
		TaskID:    taskID,
		Type:      itemType,
		RefID:     refID,
		SenderID:  senderID,
		Target:    "lead",
		Status:    InboxPending,
		CreatedAt: NowStr(),
		UpdatedAt: NowStr(),
	}
	s.store.EnsureDir("issues", issueID, "inbox", "lead")
	path := s.store.Path("issues", issueID, "inbox", "lead", item.ID+".json")
	if err := s.store.WriteJSON(path, item); err != nil {
		return nil, err
	}
	return item, nil
}

// pushToAcceptorInboxLocked adds a pending delivery item to the acceptor inbox. Call under store lock.
func (s *IssueService) pushToAcceptorInboxLocked(issueID, deliveryID, senderID string) (*InboxItem, error) {
	item := &InboxItem{
		ID:        GenID("inb"),
		IssueID:   issueID,
		TaskID:    "",
		Type:      InboxTypeDelivery,
		RefID:     deliveryID,
		SenderID:  senderID,
		Target:    "acceptor",
		Status:    InboxPending,
		CreatedAt: NowStr(),
		UpdatedAt: NowStr(),
	}
	s.store.EnsureDir("deliveries", "inbox", "acceptor")
	path := s.store.Path("deliveries", "inbox", "acceptor", item.ID+".json")
	if err := s.store.WriteJSON(path, item); err != nil {
		return nil, err
	}
	return item, nil
}

// ackAcceptorInboxByDeliveryLocked marks acceptor inbox items referencing deliveryID as done. Call under store lock.
func (s *IssueService) ackAcceptorInboxByDeliveryLocked(deliveryID string) {
	dir := s.store.Path("deliveries", "inbox", "acceptor")
	files, _ := s.store.ListJSONFiles(dir)
	for _, f := range files {
		var item InboxItem
		if err := s.store.ReadJSON(f, &item); err != nil {
			continue
		}
		if item.Type != InboxTypeDelivery || item.RefID != deliveryID || item.Status == InboxDone {
			continue
		}
		item.Status = InboxDone
		item.UpdatedAt = NowStr()
		_ = s.store.WriteJSON(f, &item)
	}
}

// claimAcceptorDeliveryInboxItemLocked claims one pending delivery inbox item for acceptor.
// It also resets stale processing claims.
// Must be called under store lock.
func (s *IssueService) claimAcceptorDeliveryInboxItemLocked(claimedBy string) (*InboxItem, error) {
	claimedBy = strings.TrimSpace(claimedBy)
	if claimedBy == "" {
		claimedBy = "acceptor"
	}
	dir := s.store.Path("deliveries", "inbox", "acceptor")
	files, err := s.store.ListJSONFiles(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	nowMs := time.Now().UnixMilli()
	for _, f := range files {
		var item InboxItem
		if err := s.store.ReadJSON(f, &item); err != nil {
			continue
		}
		if item.Type != InboxTypeDelivery {
			continue
		}
		if item.Status == InboxProcessing && item.ClaimExpiresAtMs > 0 && nowMs > item.ClaimExpiresAtMs {
			item.Status = InboxPending
			item.ClaimedBy = ""
			item.ClaimExpiresAtMs = 0
			item.UpdatedAt = NowStr()
			_ = s.store.WriteJSON(f, &item)
		}
		if item.Status != InboxPending {
			continue
		}
		item.Status = InboxProcessing
		item.ClaimedBy = claimedBy
		item.ClaimExpiresAtMs = nowMs + int64(inboxClaimTTLSec)*1000
		item.UpdatedAt = NowStr()
		if err := s.store.WriteJSON(f, &item); err != nil {
			return nil, err
		}
		copy := item
		return &copy, nil
	}
	return nil, nil
}

// claimAcceptorDeliveryInboxBlocking blocks until a delivery inbox item is available or timeout.
func (s *IssueService) claimAcceptorDeliveryInboxBlocking(claimedBy string, timeoutSec int) (*InboxItem, error) {
	deadline := s.deadline(timeoutSec)
	for {
		var item *InboxItem
		err := s.store.WithLock(func() error {
			it, err := s.claimAcceptorDeliveryInboxItemLocked(claimedBy)
			if err != nil {
				return err
			}
			item = it
			return nil
		})
		if err != nil {
			return nil, err
		}
		if item != nil {
			return item, nil
		}
		if timeExpired(deadline) {
			return nil, nil
		}
		sleepPoll()
	}
}

// pushToWorkerInboxLocked adds a pending item to a worker's inbox. Call under store lock.
func (s *IssueService) pushToWorkerInboxLocked(issueID, workerID, taskID, itemType, refID, senderID string) (*InboxItem, error) {
	item := &InboxItem{
		ID:        GenID("inb"),
		IssueID:   issueID,
		TaskID:    taskID,
		Type:      itemType,
		RefID:     refID,
		SenderID:  senderID,
		Target:    workerID,
		Status:    InboxPending,
		CreatedAt: NowStr(),
		UpdatedAt: NowStr(),
	}
	s.store.EnsureDir("issues", issueID, "inbox", "workers", workerID)
	path := s.store.Path("issues", issueID, "inbox", "workers", workerID, item.ID+".json")
	if err := s.store.WriteJSON(path, item); err != nil {
		return nil, err
	}
	return item, nil
}

// ackLeadInboxByRef marks the lead inbox item referencing refID as done. Call under store lock.
func (s *IssueService) ackLeadInboxByRefLocked(issueID, refID string) {
	dir := s.store.Path("issues", issueID, "inbox", "lead")
	files, _ := s.store.ListJSONFiles(dir)
	for _, f := range files {
		var item InboxItem
		if err := s.store.ReadJSON(f, &item); err != nil {
			continue
		}
		if item.RefID != refID || item.Status == InboxDone {
			continue
		}
		item.Status = InboxDone
		item.UpdatedAt = NowStr()
		_ = s.store.WriteJSON(f, &item)
	}
}

// claimLeadInboxItem atomically claims one pending item for the lead.
// Returns (item, nil) if found, (nil, nil) if nothing pending, (nil, err) on error.
// Items in "processing" with expired claims are reset to "pending" first.
func (s *IssueService) claimLeadInboxItem(issueID, claimedBy string) (*InboxItem, error) {
	var result *InboxItem
	err := s.store.WithLock(func() error {
		dir := s.store.Path("issues", issueID, "inbox", "lead")
		files, err := s.store.ListJSONFiles(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		nowMs := time.Now().UnixMilli()
		for _, f := range files {
			var item InboxItem
			if err := s.store.ReadJSON(f, &item); err != nil {
				continue
			}
			// Reset stale processing claims
			if item.Status == InboxProcessing && item.ClaimExpiresAtMs > 0 && nowMs > item.ClaimExpiresAtMs {
				item.Status = InboxPending
				item.ClaimedBy = ""
				item.ClaimExpiresAtMs = 0
				item.UpdatedAt = NowStr()
				_ = s.store.WriteJSON(f, &item)
			}
			if item.Status == InboxPending && result == nil {
				item.Status = InboxProcessing
				item.ClaimedBy = claimedBy
				item.ClaimExpiresAtMs = nowMs + int64(inboxClaimTTLSec)*1000
				item.UpdatedAt = NowStr()
				if err := s.store.WriteJSON(f, &item); err != nil {
					return err
				}
				itemCopy := item
				result = &itemCopy
			}
		}
		return nil
	})
	return result, err
}

// deleteInboxForTaskLocked removes all inbox items (lead + worker) for a task. Call under store lock.
func (s *IssueService) deleteInboxForTaskLocked(issueID, taskID string) {
	// Lead inbox
	leadDir := s.store.Path("issues", issueID, "inbox", "lead")
	for _, f := range listJSONOrEmpty(s.store, leadDir) {
		var item InboxItem
		if err := s.store.ReadJSON(f, &item); err != nil {
			continue
		}
		if item.TaskID == taskID {
			_ = s.store.Remove(f)
		}
	}
	// Worker inboxes
	workersDir := s.store.Path("issues", issueID, "inbox", "workers")
	entries, err := os.ReadDir(workersDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		workerDir := s.store.Path("issues", issueID, "inbox", "workers", e.Name())
		for _, f := range listJSONOrEmpty(s.store, workerDir) {
			var item InboxItem
			if err := s.store.ReadJSON(f, &item); err != nil {
				continue
			}
			if item.TaskID == taskID {
				_ = s.store.Remove(f)
			}
		}
	}
}

// claimLeadInboxBlocking polls until a lead inbox item is available or timeout.
func (s *IssueService) claimLeadInboxBlocking(issueID, claimedBy string, timeoutSec int) (*InboxItem, error) {
	deadline := s.deadline(timeoutSec)
	for {
		item, err := s.claimLeadInboxItem(issueID, claimedBy)
		if err != nil {
			return nil, err
		}
		if item != nil {
			return item, nil
		}
		if timeExpired(deadline) {
			return nil, nil // timeout, no items — caller returns empty
		}
		sleepPoll()
	}
}

// materializeInboxItemAsEvent converts a claimed inbox item into an IssueEvent-shaped map
// suitable for the waitIssueTaskEvents response. Loads the referenced entity for full content.
func (s *IssueService) materializeInboxItem(issueID string, item *InboxItem) map[string]any {
	base := map[string]any{
		"seq":       -1, // not event-seq based
		"issue_id":  issueID,
		"task_id":   item.TaskID,
		"actor":     item.SenderID,
		"refs":      "",
		"timestamp": item.CreatedAt,
		"inbox_id":  item.ID,
	}
	switch item.Type {
	case InboxTypeQuestion, InboxTypeBlocker:
		base["type"] = EventIssueTaskMessage
		base["kind"] = item.Type
		base["message_id"] = item.RefID
		// Load message content
		var msg TaskMessage
		path := s.store.Path("issues", issueID, "messages", item.RefID+".json")
		if err := s.store.ReadJSON(path, &msg); err == nil {
			base["detail"] = msg.Content
			base["refs"] = msg.Refs
			base["timestamp"] = msg.CreatedAt
		}
	case InboxTypeSubmission:
		base["type"] = EventSubmissionCreated
		base["kind"] = ""
		base["detail"] = "submitted"
		base["submission_id"] = item.RefID
		// Load submission artifacts
		var sub Submission
		_ = s.store.WithLock(func() error {
			found, err := s.getSubmissionLocked(issueID, item.RefID)
			if err == nil {
				sub = *found
			}
			return nil
		})
		if sub.ID != "" {
			base["submission_artifacts"] = sub.Artifacts
			base["timestamp"] = sub.CreatedAt
		}
	}
	return base
}

// listJSONOrEmpty lists JSON files in dir, returning empty on error.
func listJSONOrEmpty(store *Store, dir string) []string {
	files, _ := store.ListJSONFiles(dir)
	return files
}

// sweepInboxClaims resets stale processing claims back to pending for the given issue.
func (s *IssueService) sweepInboxClaims(issueID string) {
	dir := s.store.Path("issues", issueID, "inbox", "lead")
	files, _ := s.store.ListJSONFiles(dir)
	nowMs := time.Now().UnixMilli()
	_ = s.store.WithLock(func() error {
		for _, f := range files {
			var item InboxItem
			if err := s.store.ReadJSON(f, &item); err != nil {
				continue
			}
			if item.Status == InboxProcessing && item.ClaimExpiresAtMs > 0 && nowMs > item.ClaimExpiresAtMs {
				item.Status = InboxPending
				item.ClaimedBy = ""
				item.ClaimExpiresAtMs = 0
				item.UpdatedAt = NowStr()
				_ = s.store.WriteJSON(f, &item)
			}
		}
		return nil
	})
}
