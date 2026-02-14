//go:build legacy
// +build legacy

package swarm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type TeamService struct {
	store *Store
	trace *TraceService
}

func NewTeamService(store *Store, trace *TraceService) *TeamService {
	return &TeamService{store: store, trace: trace}
}

func (s *TeamService) SpawnTeam(team string, members []string) (*TeamConfig, error) {
	if team == "" {
		return nil, fmt.Errorf("team name is required")
	}

	var result *TeamConfig
	err := s.store.WithLock(func() error {
		cfgPath := s.store.Path("teams", team, "config.json")
		if s.store.Exists("teams", team, "config.json") {
			return fmt.Errorf("team '%s' already exists", team)
		}

		cfg := &TeamConfig{
			Team:      team,
			Members:   members,
			CreatedAt: NowStr(),
		}

		s.store.EnsureDir("teams", team, "tasks")
		for _, m := range members {
			s.store.EnsureDir("teams", team, "inbox", m)
		}

		if err := s.store.WriteJSON(cfgPath, cfg); err != nil {
			return err
		}

		result = cfg
		return nil
	})

	if err == nil {
		s.trace.Log(TraceEvent{
			Type:    EventTeamCreated,
			Team:    team,
			Actor:   "system",
			Subject: team,
			Detail:  fmt.Sprintf("members: %v", members),
		})
	}

	return result, err
}

func (s *TeamService) ListTeams() ([]TeamConfig, error) {
	dir := s.store.Path("teams")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []TeamConfig{}, nil
		}
		return nil, err
	}

	var teams []TeamConfig
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		cfgPath := filepath.Join(dir, e.Name(), "config.json")
		var cfg TeamConfig
		if err := s.store.ReadJSON(cfgPath, &cfg); err != nil {
			continue
		}
		teams = append(teams, cfg)
	}
	return teams, nil
}

func (s *TeamService) ListMembers(team string) ([]string, error) {
	cfgPath := s.store.Path("teams", team, "config.json")
	var cfg TeamConfig
	if err := s.store.ReadJSON(cfgPath, &cfg); err != nil {
		return nil, fmt.Errorf("team '%s' not found", team)
	}
	return cfg.Members, nil
}

func (s *TeamService) JoinTeam(team, member string) (*TeamConfig, error) {
	if team == "" || member == "" {
		return nil, fmt.Errorf("team and member are required")
	}

	cfgPath := s.store.Path("teams", team, "config.json")
	var result *TeamConfig

	err := s.store.WithLock(func() error {
		var cfg TeamConfig
		if err := s.store.ReadJSON(cfgPath, &cfg); err != nil {
			return fmt.Errorf("team '%s' not found", team)
		}

		found := false
		for _, m := range cfg.Members {
			if m == member {
				found = true
				break
			}
		}
		if !found {
			cfg.Members = append(cfg.Members, member)
		}

		if err := s.store.WriteJSON(cfgPath, &cfg); err != nil {
			return err
		}

		s.store.EnsureDir("teams", team, "inbox", member)
		result = &cfg
		return nil
	})

	if err == nil {
		s.trace.Log(TraceEvent{
			Type:    EventTeamMemberJoined,
			Team:    team,
			Actor:   member,
			Subject: member,
		})
	}

	return result, err
}

func (s *TeamService) TeamExists(team string) bool {
	return s.store.Exists("teams", team, "config.json")
}
