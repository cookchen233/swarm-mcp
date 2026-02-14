package swarm

import (
	"fmt"
	"sort"
	"time"
)

func (s *IssueService) loadIssueWorkerStateLocked(issueID, workerID string) (*IssueWorkerState, error) {
	if issueID == "" || workerID == "" {
		return nil, fmt.Errorf("issue_id and worker_id are required")
	}
	path := s.store.Path("issues", issueID, "workers", workerID+".json")
	var st IssueWorkerState
	if err := s.store.ReadJSON(path, &st); err == nil {
		return &st, nil
	}
	return &IssueWorkerState{IssueID: issueID, WorkerID: workerID, TotalPoints: 0, ConsecutiveLowScores: 0, UpdatedAt: NowStr()}, nil
}

func (s *IssueService) saveIssueWorkerStateLocked(st *IssueWorkerState) error {
	if st == nil {
		return fmt.Errorf("worker state is nil")
	}
	s.store.EnsureDir("issues", st.IssueID, "workers")
	st.UpdatedAt = NowStr()
	path := s.store.Path("issues", st.IssueID, "workers", st.WorkerID+".json")
	return s.store.WriteJSON(path, st)
}

func downgradeDifficulty(d string) string {
	switch d {
	case "focus":
		return "medium"
	case "medium":
		return "easy"
	default:
		return "easy"
	}
}

func baseDifficultyByPoints(total int) string {
	if total >= 30 {
		return "focus"
	}
	if total >= 10 {
		return "medium"
	}
	return "easy"
}

func difficultyFallbackOrder(d string) []string {
	switch d {
	case "focus":
		return []string{"focus", "medium", "easy"}
	case "medium":
		return []string{"medium", "easy"}
	default:
		return []string{"easy"}
	}
}

func pickTaskByTier(tasks []*IssueTask, totalPoints int) *IssueTask {
	if len(tasks) == 0 {
		return nil
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Points != tasks[j].Points {
			return tasks[i].Points > tasks[j].Points
		}
		return tasks[i].ID < tasks[j].ID
	})
	if totalPoints >= 100 {
		return tasks[0]
	}
	if totalPoints >= 30 && totalPoints < 50 {
		return tasks[len(tasks)-1]
	}
	return tasks[len(tasks)/2]
}

func (s *IssueService) GetNextStepToken(issueID, actor, justFinishedTaskID, workerID string, completionScore int) (map[string]any, error) {
	if issueID == "" || workerID == "" || justFinishedTaskID == "" {
		return nil, fmt.Errorf("issue_id, task_id and worker_id are required")
	}
	if actor == "" {
		actor = "lead"
	}
	if completionScore != 1 && completionScore != 2 && completionScore != 5 {
		return nil, fmt.Errorf("invalid completion_score: %d", completionScore)
	}

	const (
		bufferLevel1 = 50
		bufferLevel2 = 100
	)

	var out map[string]any
	err := s.store.WithLock(func() error {
		st, err := s.loadIssueWorkerStateLocked(issueID, workerID)
		if err != nil {
			return err
		}
		finished, err := s.loadTaskLocked(issueID, justFinishedTaskID)
		if err != nil {
			return err
		}
		st.TotalPoints += finished.Points

		base := baseDifficultyByPoints(st.TotalPoints)
		nextDifficulty := base

		if completionScore < 2 {
			st.ConsecutiveLowScores++
			allowedFailures := 0
			if st.TotalPoints >= bufferLevel2 {
				allowedFailures = 2
			} else if st.TotalPoints >= bufferLevel1 {
				allowedFailures = 1
			}
			if st.ConsecutiveLowScores > allowedFailures {
				nextDifficulty = downgradeDifficulty(base)
			}
		} else {
			st.ConsecutiveLowScores = 0
		}

		if err := s.saveIssueWorkerStateLocked(st); err != nil {
			return err
		}

		var chosen *IssueTask
		for _, d := range difficultyFallbackOrder(nextDifficulty) {
			tasksDir := s.store.Path("issues", issueID, "tasks")
			files, err := s.store.ListJSONFiles(tasksDir)
			if err != nil {
				return err
			}
			candidates := make([]*IssueTask, 0)
			for _, f := range files {
				var t IssueTask
				if err := s.store.ReadJSON(f, &t); err != nil {
					continue
				}
				if t.Status != IssueTaskOpen || t.Difficulty != d {
					continue
				}
				candidates = append(candidates, &t)
			}
			chosen = pickTaskByTier(candidates, st.TotalPoints)
			if chosen != nil {
				break
			}
		}

		tok := NextStepToken{Token: GenID("ns"), IssueID: issueID, Actor: actor, Attached: false, Used: false, CreatedAt: NowStr()}
		if chosen == nil {
			tok.NextStep = NextStep{Type: "end"}
			path := s.store.Path("issues", issueID, "next_steps", tok.Token+".json")
			if err := s.store.WriteJSON(path, tok); err != nil {
				return err
			}
			out = map[string]any{"next_step_token": tok.Token, "next_step": tok.NextStep, "difficulty": nextDifficulty, "worker_total_points": st.TotalPoints, "consecutive_low_scores": st.ConsecutiveLowScores}
			return nil
		}

		nowMs := time.Now().UnixMilli()
		const reserveTTL = int64(2 * 60 * 1000)
		live, err := s.loadTaskLocked(issueID, chosen.ID)
		if err != nil {
			return err
		}
		if live.Status != IssueTaskOpen {
			return fmt.Errorf("next_step task '%s' is not open (status: %s)", live.ID, live.Status)
		}
		if live.ReservedToken != "" && live.ReservedUntilMs > 0 && nowMs <= live.ReservedUntilMs {
			return fmt.Errorf("next_step task '%s' is reserved", live.ID)
		}

		tok.NextStep = NextStep{Type: "claim_task", TaskID: live.ID}
		path := s.store.Path("issues", issueID, "next_steps", tok.Token+".json")
		if err := s.store.WriteJSON(path, tok); err != nil {
			return err
		}

		live.ReservedToken = tok.Token
		live.ReservedUntilMs = nowMs + reserveTTL
		live.UpdatedAt = NowStr()
		if err := s.store.WriteJSON(s.store.Path("issues", issueID, "tasks", live.ID+".json"), live); err != nil {
			return err
		}

		out = map[string]any{"next_step_token": tok.Token, "next_step": tok.NextStep, "difficulty": nextDifficulty, "worker_total_points": st.TotalPoints, "consecutive_low_scores": st.ConsecutiveLowScores}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *IssueService) ReadNextStepToken(issueID, token string) (*NextStepToken, error) {
	if issueID == "" || token == "" {
		return nil, fmt.Errorf("issue_id and token are required")
	}
	var out *NextStepToken
	err := s.store.WithLock(func() error {
		var tok NextStepToken
		if err := s.store.ReadJSON(s.store.Path("issues", issueID, "next_steps", token+".json"), &tok); err != nil {
			return err
		}
		out = &tok
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
