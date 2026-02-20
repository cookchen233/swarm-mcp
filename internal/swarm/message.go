package swarm

import (
	"fmt"
	"os"
	"strings"
)

// createTaskMessageLocked creates a TaskMessage entity. Must be called under store lock.
func (s *IssueService) createTaskMessageLocked(issueID, taskID, senderID, kind, content, refs string) (*TaskMessage, error) {
	msg := &TaskMessage{
		ID:        GenID("msg"),
		IssueID:   issueID,
		TaskID:    taskID,
		SenderID:  senderID,
		Kind:      kind,
		Content:   content,
		Refs:      refs,
		Status:    MessageOpen,
		CreatedAt: NowStr(),
		UpdatedAt: NowStr(),
	}
	s.store.EnsureDir("issues", issueID, "messages")
	path := s.store.Path("issues", issueID, "messages", msg.ID+".json")
	if err := s.store.WriteJSON(path, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// getTaskMessageLocked reads a TaskMessage by ID. Must be called under store lock.
func (s *IssueService) getTaskMessageLocked(issueID, messageID string) (*TaskMessage, error) {
	path := s.store.Path("issues", issueID, "messages", messageID+".json")
	var msg TaskMessage
	if err := s.store.ReadJSON(path, &msg); err != nil {
		return nil, fmt.Errorf("message '%s' not found", messageID)
	}
	return &msg, nil
}

// replyTaskMessageLocked marks a message as replied and stores the reply. Must be called under store lock.
func (s *IssueService) replyTaskMessageLocked(issueID, messageID, actor, content, refs string) (*TaskMessage, error) {
	msg, err := s.getTaskMessageLocked(issueID, messageID)
	if err != nil {
		return nil, err
	}
	if msg.Status == MessageReplied || msg.Status == MessageResolved {
		return nil, fmt.Errorf("message '%s' already has a reply (status: %s)", messageID, msg.Status)
	}
	msg.Status = MessageReplied
	msg.ReplyContent = content
	msg.ReplyBy = actor
	msg.RepliedAt = NowStr()
	msg.UpdatedAt = NowStr()
	if refs != "" {
		msg.Refs = refs
	}
	path := s.store.Path("issues", issueID, "messages", msg.ID+".json")
	if err := s.store.WriteJSON(path, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// GetTaskMessage returns a single TaskMessage by ID.
func (s *IssueService) GetTaskMessage(issueID, messageID string) (*TaskMessage, error) {
	var result *TaskMessage
	err := s.store.WithLock(func() error {
		msg, err := s.getTaskMessageLocked(issueID, messageID)
		if err != nil {
			return err
		}
		result = msg
		return nil
	})
	return result, err
}

// ListTaskMessages returns all messages for an issue (optionally filtered by taskID).
func (s *IssueService) ListTaskMessages(issueID, taskID string) ([]TaskMessage, error) {
	dir := s.store.Path("issues", issueID, "messages")
	files, err := s.store.ListJSONFiles(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []TaskMessage{}, nil
		}
		return nil, err
	}
	var out []TaskMessage
	for _, f := range files {
		var msg TaskMessage
		if err := s.store.ReadJSON(f, &msg); err != nil {
			continue
		}
		if taskID != "" && msg.TaskID != taskID {
			continue
		}
		out = append(out, msg)
	}
	return out, nil
}

// deleteMessagesForTaskLocked removes all message files for a task. Call under store lock.
func (s *IssueService) deleteMessagesForTaskLocked(issueID, taskID string) {
	dir := s.store.Path("issues", issueID, "messages")
	files, _ := s.store.ListJSONFiles(dir)
	for _, f := range files {
		var msg TaskMessage
		if err := s.store.ReadJSON(f, &msg); err != nil {
			continue
		}
		if msg.TaskID != taskID {
			continue
		}
		_ = s.store.Remove(f)
	}
}

// pollMessageReply polls until the message has a reply. Used by AskIssueTask blocking wait.
func (s *IssueService) pollMessageReply(issueID, messageID string, timeoutSec int) (*TaskMessage, error) {
	deadline := s.deadline(timeoutSec)
	for {
		var msg *TaskMessage
		_ = s.store.WithLock(func() error {
			found, err := s.getTaskMessageLocked(issueID, messageID)
			if err == nil {
				msg = found
			}
			return nil
		})
		if msg != nil && (msg.Status == MessageReplied || msg.Status == MessageResolved) {
			return msg, nil
		}
		if timeExpired(deadline) {
			return nil, fmt.Errorf("timeout waiting for reply to message '%s'", messageID)
		}
		sleepPoll()
	}
}

// resolveMessageForReply finds the message to reply to. If messageID is given, use it.
// Otherwise find the oldest open message for the task.
func (s *IssueService) resolveMessageForReply(issueID, taskID, messageID string) (*TaskMessage, error) {
	if strings.TrimSpace(messageID) != "" {
		return s.getTaskMessageLocked(issueID, messageID)
	}
	// Find oldest open message for this task
	dir := s.store.Path("issues", issueID, "messages")
	files, _ := s.store.ListJSONFiles(dir)
	var oldest *TaskMessage
	for _, f := range files {
		var msg TaskMessage
		if err := s.store.ReadJSON(f, &msg); err != nil {
			continue
		}
		if msg.TaskID != taskID || msg.Status != MessageOpen {
			continue
		}
		if oldest == nil || msg.CreatedAt < oldest.CreatedAt {
			oldest = &msg
		}
	}
	if oldest == nil {
		return nil, fmt.Errorf("no open message found for task '%s'", taskID)
	}
	return oldest, nil
}
