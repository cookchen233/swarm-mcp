package swarm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type TraceService struct {
	store *Store
}

func NewTraceService(store *Store) *TraceService {
	return &TraceService{store: store}
}

func (t *TraceService) Log(event TraceEvent) {
	if event.ID == "" {
		event.ID = GenID("ev")
	}
	if event.Timestamp == "" {
		event.Timestamp = NowStr()
	}

	dir := t.store.EnsureDir("trace")
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	data, _ := json.Marshal(event)
	fmt.Fprintln(f, string(data))
}
