//go:build legacy
// +build legacy

package swarm

import (
	"fmt"
	"sort"
)

type MessageService struct {
	store *Store
	trace *TraceService
}

func NewMessageService(store *Store, trace *TraceService) *MessageService {
	return &MessageService{store: store, trace: trace}
}

func (s *MessageService) SendMessage(team, from, to, content, refs string) (*Message, error) {
	if team == "" || from == "" || to == "" || content == "" {
		return nil, fmt.Errorf("team, from, to, and content are required")
	}

	msg := &Message{
		ID:        GenID("msg"),
		Team:      team,
		From:      from,
		To:        to,
		Content:   content,
		Refs:      refs,
		CreatedAt: NowStr(),
	}

	err := s.store.WithLock(func() error {
		dir := s.store.EnsureDir("teams", team, "inbox", to)
		path := s.store.Path("teams", team, "inbox", to, msg.ID+".json")
		_ = dir
		return s.store.WriteJSON(path, msg)
	})

	if err == nil {
		s.trace.Log(TraceEvent{
			Type:    EventMessageSent,
			Team:    team,
			Actor:   from,
			Subject: to,
			Detail:  content,
		})
	}

	return msg, err
}

func (s *MessageService) BroadcastMessage(team, from, content, refs string) (*Message, error) {
	if team == "" || from == "" || content == "" {
		return nil, fmt.Errorf("team, from, and content are required")
	}

	// Load team members
	cfgPath := s.store.Path("teams", team, "config.json")
	var cfg TeamConfig
	if err := s.store.ReadJSON(cfgPath, &cfg); err != nil {
		return nil, fmt.Errorf("team '%s' not found", team)
	}

	msg := &Message{
		ID:        GenID("msg"),
		Team:      team,
		From:      from,
		To:        "",
		Content:   content,
		Refs:      refs,
		CreatedAt: NowStr(),
	}

	err := s.store.WithLock(func() error {
		for _, member := range cfg.Members {
			if member == from {
				continue
			}
			s.store.EnsureDir("teams", team, "inbox", member)
			path := s.store.Path("teams", team, "inbox", member, msg.ID+".json")
			if err := s.store.WriteJSON(path, msg); err != nil {
				return err
			}
		}
		return nil
	})

	if err == nil {
		s.trace.Log(TraceEvent{
			Type:    EventMessageSent,
			Team:    team,
			Actor:   from,
			Subject: "broadcast",
			Detail:  content,
		})
	}

	return msg, err
}

func (s *MessageService) ReadInbox(team, member string, limit int) ([]Message, error) {
	if team == "" || member == "" {
		return nil, fmt.Errorf("team and member are required")
	}
	if limit <= 0 {
		limit = 50
	}

	dir := s.store.Path("teams", team, "inbox", member)
	files, err := s.store.ListJSONFiles(dir)
	if err != nil {
		return nil, err
	}

	// Sort by filename (contains timestamp-based ID) descending for newest first
	sort.Sort(sort.Reverse(sort.StringSlice(files)))

	var messages []Message
	for i, f := range files {
		if i >= limit {
			break
		}
		var msg Message
		if err := s.store.ReadJSON(f, &msg); err != nil {
			continue
		}
		messages = append(messages, msg)
	}

	return messages, nil
}
