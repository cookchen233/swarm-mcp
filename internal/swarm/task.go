//go:build legacy
// +build legacy

package swarm

import (
	"fmt"
	"strings"
)

type TaskService struct {
	store *Store
	trace *TraceService
}

func NewTaskService(store *Store, trace *TraceService) *TaskService {
	return &TaskService{store: store, trace: trace}
}

func (s *TaskService) CreateTask(team, subject, description string, suggestedFiles, labels, deps []string) (*Task, error) {
	if team == "" || subject == "" {
		return nil, fmt.Errorf("team and subject are required")
	}

	var result *Task
	err := s.store.WithLock(func() error {
		task := &Task{
			ID:             GenID("t"),
			Team:           team,
			Subject:        subject,
			Description:    description,
			SuggestedFiles: suggestedFiles,
			Labels:         labels,
			Deps:           deps,
			Status:         TaskOpen,
			CreatedAt:      NowStr(),
			UpdatedAt:      NowStr(),
		}

		s.store.EnsureDir("teams", team, "tasks")
		path := s.store.Path("teams", team, "tasks", task.ID+".json")
		if err := s.store.WriteJSON(path, task); err != nil {
			return err
		}
		result = task
		return nil
	})

	if err == nil {
		s.trace.Log(TraceEvent{
			Type:    EventTaskCreated,
			Team:    team,
			Actor:   "lead",
			Subject: result.ID,
			Detail:  subject,
		})
	}

	return result, err
}

func (s *TaskService) AssignTask(team, taskID, to string) (*Task, error) {
	if taskID == "" || to == "" {
		return nil, fmt.Errorf("task_id and to are required")
	}

	var result *Task
	err := s.store.WithLock(func() error {
		task, err := s.loadTask(team, taskID)
		if err != nil {
			return err
		}
		task.Assignee = to
		task.UpdatedAt = NowStr()

		return s.saveTask(task)
	})

	if err == nil {
		result, _ = s.loadTask(team, taskID)
		s.trace.Log(TraceEvent{
			Type:    EventTaskAssigned,
			Team:    team,
			Actor:   "lead",
			Subject: taskID,
			Detail:  fmt.Sprintf("assigned to %s", to),
		})
	}

	return result, err
}

func (s *TaskService) ClaimTask(team, taskID, member string) (*Task, error) {
	if taskID == "" || member == "" {
		return nil, fmt.Errorf("task_id and member are required")
	}

	var result *Task
	err := s.store.WithLock(func() error {
		task, err := s.loadTask(team, taskID)
		if err != nil {
			return err
		}
		if task.Status != TaskOpen {
			return fmt.Errorf("task '%s' is not open (status: %s)", taskID, task.Status)
		}
		task.ClaimedBy = member
		task.Status = TaskInProgress
		task.UpdatedAt = NowStr()
		result = task
		return s.saveTask(task)
	})

	if err == nil {
		s.trace.Log(TraceEvent{
			Type:    EventTaskClaimed,
			Team:    team,
			Actor:   member,
			Subject: taskID,
		})
	}

	return result, err
}

func (s *TaskService) UpdateTask(team, taskID, status, summary string, touchedFiles []string) (*Task, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task_id is required")
	}

	validStatuses := map[string]bool{
		TaskOpen: true, TaskInProgress: true, TaskVerifying: true,
		TaskBlocked: true, TaskDone: true, TaskCanceled: true,
	}
	if status != "" && !validStatuses[status] {
		return nil, fmt.Errorf("invalid status: %s", status)
	}

	var result *Task
	err := s.store.WithLock(func() error {
		task, err := s.loadTask(team, taskID)
		if err != nil {
			return err
		}
		if status != "" {
			task.Status = status
		}
		if summary != "" {
			task.Summary = summary
		}
		if len(touchedFiles) > 0 {
			task.TouchedFiles = touchedFiles
		}
		task.UpdatedAt = NowStr()
		result = task
		return s.saveTask(task)
	})

	if err == nil {
		s.trace.Log(TraceEvent{
			Type:    EventTaskUpdated,
			Team:    team,
			Actor:   result.ClaimedBy,
			Subject: taskID,
			Detail:  fmt.Sprintf("status=%s", result.Status),
		})
	}

	return result, err
}

func (s *TaskService) ListTasks(team, status, assignee string) ([]Task, error) {
	dir := s.store.Path("teams", team, "tasks")
	files, err := s.store.ListJSONFiles(dir)
	if err != nil {
		return nil, err
	}

	var tasks []Task
	for _, f := range files {
		var t Task
		if err := s.store.ReadJSON(f, &t); err != nil {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		if assignee != "" && t.Assignee != assignee && t.ClaimedBy != assignee {
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (s *TaskService) loadTask(team, taskID string) (*Task, error) {
	// Try exact path first
	path := s.store.Path("teams", team, "tasks", taskID+".json")
	var task Task
	if err := s.store.ReadJSON(path, &task); err != nil {
		// Try scanning for partial match
		dir := s.store.Path("teams", team, "tasks")
		files, _ := s.store.ListJSONFiles(dir)
		for _, f := range files {
			if strings.Contains(f, taskID) {
				if err := s.store.ReadJSON(f, &task); err == nil {
					return &task, nil
				}
			}
		}
		return nil, fmt.Errorf("task '%s' not found in team '%s'", taskID, team)
	}
	return &task, nil
}

func (s *TaskService) saveTask(task *Task) error {
	path := s.store.Path("teams", task.Team, "tasks", task.ID+".json")
	return s.store.WriteJSON(path, task)
}
