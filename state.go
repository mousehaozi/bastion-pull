package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type projectState struct {
	ReleaseID  int64     `json:"release_id"`
	Tag        string    `json:"tag"`
	DeployedAt time.Time `json:"deployed_at"`
}

type State struct {
	Projects map[string]projectState `json:"projects"`
}

func loadState(path string) (*State, error) {
	s := &State{Projects: map[string]projectState{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	if s.Projects == nil {
		s.Projects = map[string]projectState{}
	}
	return s, nil
}

func (s *State) save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
