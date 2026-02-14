package swarm

import (
	"fmt"
)

type WorkerService struct {
	store *Store
	trace *TraceService
}

func NewWorkerService(store *Store, trace *TraceService) *WorkerService {
	return &WorkerService{store: store, trace: trace}
}

func (w *WorkerService) Register(workerID string) (*Worker, error) {
	if workerID == "" {
		return nil, fmt.Errorf("worker_id is required")
	}

	var result *Worker
	err := w.store.WithLock(func() error {
		w.store.EnsureDir("workers")
		path := w.store.Path("workers", workerID+".json")

		var existing Worker
		if err := w.store.ReadJSON(path, &existing); err == nil {
			existing.UpdatedAt = NowStr()
			if err := w.store.WriteJSON(path, &existing); err != nil {
				return err
			}
			result = &existing
			return nil
		}

		worker := &Worker{ID: workerID, JoinedAt: NowStr(), UpdatedAt: NowStr()}
		if err := w.store.WriteJSON(path, worker); err != nil {
			return err
		}
		result = worker
		return nil
	})

	if err == nil {
		w.trace.Log(TraceEvent{Type: EventWorkerRegistered, Actor: workerID, Subject: workerID})
	}
	return result, err
}

func (w *WorkerService) Get(workerID string) (*Worker, error) {
	if workerID == "" {
		return nil, fmt.Errorf("worker_id is required")
	}
	path := w.store.Path("workers", workerID+".json")
	var worker Worker
	if err := w.store.ReadJSON(path, &worker); err != nil {
		return nil, err
	}
	return &worker, nil
}

func (w *WorkerService) List() ([]Worker, error) {
	dir := w.store.Path("workers")
	files, err := w.store.ListJSONFiles(dir)
	if err != nil {
		return []Worker{}, nil
	}

	out := make([]Worker, 0, len(files))
	for _, f := range files {
		var worker Worker
		if err := w.store.ReadJSON(f, &worker); err != nil {
			continue
		}
		out = append(out, worker)
	}
	return out, nil
}
