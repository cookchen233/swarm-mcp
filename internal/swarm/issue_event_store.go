package swarm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func (s *IssueService) ReadAllEvents(issueID string) ([]IssueEvent, error) {
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}
	if !s.store.Exists("issues", issueID, "issue.json") {
		return nil, fmt.Errorf("issue '%s' not found", issueID)
	}

	eventsPath := s.store.Path("issues", issueID, "events.jsonl")
	f, err := os.Open(eventsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []IssueEvent{}, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)

	out := make([]IssueEvent, 0, 64)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev IssueEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *IssueService) appendEventLocked(issueID string, ev IssueEvent) error {
	metaPath := s.store.Path("issues", issueID, "meta.json")
	var meta issueMeta
	if err := s.store.ReadJSON(metaPath, &meta); err != nil {
		return err
	}
	ev.Seq = meta.NextSeq
	meta.NextSeq++
	if err := s.store.WriteJSON(metaPath, &meta); err != nil {
		return err
	}

	eventsPath := s.store.Path("issues", issueID, "events.jsonl")
	_ = os.MkdirAll(filepath.Dir(eventsPath), 0755)
	f, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}

	return nil
}

func (s *IssueService) appendEventLockedWithSeq(issueID string, ev *IssueEvent) (int64, error) {
	metaPath := s.store.Path("issues", issueID, "meta.json")
	var meta issueMeta
	if err := s.store.ReadJSON(metaPath, &meta); err != nil {
		return 0, err
	}

	ev.Seq = meta.NextSeq
	meta.NextSeq++
	if err := s.store.WriteJSON(metaPath, &meta); err != nil {
		return 0, err
	}

	eventsPath := s.store.Path("issues", issueID, "events.jsonl")
	_ = os.MkdirAll(filepath.Dir(eventsPath), 0755)
	f, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	b, err := json.Marshal(ev)
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return 0, err
	}

	return ev.Seq, nil
}

func (s *IssueService) readEventsAfter(issueID string, afterSeq int64, limit int) ([]IssueEvent, int64, error) {
	if !s.store.Exists("issues", issueID, "issue.json") {
		return nil, afterSeq, fmt.Errorf("issue '%s' not found", issueID)
	}

	eventsPath := s.store.Path("issues", issueID, "events.jsonl")
	f, err := os.Open(eventsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, afterSeq, nil
		}
		return nil, afterSeq, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)

	var out []IssueEvent
	nextSeq := afterSeq
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev IssueEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Seq <= afterSeq {
			continue
		}
		out = append(out, ev)
		nextSeq = ev.Seq
		if len(out) >= limit {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, afterSeq, err
	}

	return out, nextSeq, nil
}

func (s *IssueService) readTaskEventsAfter(issueID, taskID string, afterSeq int64, limit int) ([]IssueEvent, int64, error) {
	if !s.store.Exists("issues", issueID, "issue.json") {
		return nil, afterSeq, fmt.Errorf("issue '%s' not found", issueID)
	}

	eventsPath := s.store.Path("issues", issueID, "events.jsonl")
	f, err := os.Open(eventsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, afterSeq, nil
		}
		return nil, afterSeq, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)

	var out []IssueEvent
	nextSeq := afterSeq
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev IssueEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Seq <= afterSeq {
			continue
		}
		if ev.TaskID != taskID {
			continue
		}
		out = append(out, ev)
		nextSeq = ev.Seq
		if len(out) >= limit {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, afterSeq, err
	}

	return out, nextSeq, nil
}
