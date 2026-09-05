// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package localstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"gitea.dev/contrib/gitea-stack/internal/gitx"
	"gitea.dev/modules/json"
)

type Layer struct {
	Branch      string `json:"branch"`
	PullRequest int64  `json:"pr,omitempty"`
	HeadSHA     string `json:"head_sha"`
	ParentSHA   string `json:"parent_sha"`
	RemoteSHA   string `json:"remote_sha,omitempty"`
	LandedSHA   string `json:"landed_sha,omitempty"`
}

type State struct {
	Remote             string  `json:"remote"`
	Trunk              string  `json:"trunk"`
	Stack              int64   `json:"stack,omitempty"`
	Layers             []Layer `json:"layers"`
	LastRevision       int64   `json:"last_revision,omitempty"`
	LastSyncedTrunkSHA string  `json:"last_synced_trunk_sha"`
}

type RestackLayer struct {
	Branch       string `json:"branch"`
	OldBase      string `json:"old_base"`
	NewBase      string `json:"new_base"`
	OriginalHead string `json:"original_head"`
	NewHead      string `json:"new_head,omitempty"`
	State        string `json:"state"`
}

type Restack struct {
	Phase          string         `json:"phase"`
	Stack          int64          `json:"stack,omitempty"`
	Trunk          string         `json:"trunk"`
	Sign           string         `json:"sign,omitempty"`
	Snapshot       string         `json:"snapshot"`
	OriginalBranch string         `json:"original_branch"`
	Current        int            `json:"current"`
	Layers         []RestackLayer `json:"layers"`
}

type Store struct{ Dir string }

func Open(repo gitx.Repo) (*Store, error) {
	dir, err := repo.GitPath("gitea-stack")
	if err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) path(name string) string { return filepath.Join(s.Dir, name) }

func read[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	value := new(T)
	if err := json.Unmarshal(data, value); err != nil {
		return nil, err
	}
	return value, nil
}

func (s *Store) Load() (*State, error) { return read[State](s.path("stack.json")) }

func (s *Store) LoadRestack() (*Restack, error) { return read[Restack](s.path("restack.json")) }

func write(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func (s *Store) Save(state *State) error          { return write(s.path("stack.json"), state) }
func (s *Store) SaveRestack(state *Restack) error { return write(s.path("restack.json"), state) }
func (s *Store) Remove() error                    { return os.Remove(s.path("stack.json")) }
func (s *Store) RemoveRestack() error             { return os.Remove(s.path("restack.json")) }
func (s *Store) RestackExists() bool              { _, err := os.Stat(s.path("restack.json")); return err == nil }
func (s *Store) RestackPath() string              { return s.path("restack.json") }
func (s *Store) StatePath() string                { return s.path("stack.json") }

func (s *Store) Lock() (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	path := s.path("operation.lock")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			data, readErr := os.ReadFile(path)
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if readErr == nil && parseErr == nil {
				process, findErr := os.FindProcess(pid)
				if findErr == nil {
					signalErr := process.Signal(syscall.Signal(0))
					if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) && !errors.Is(signalErr, syscall.ESRCH) {
						return nil, errors.New("another gitea-stack operation is running")
					}
					if signalErr == nil {
						return nil, errors.New("another gitea-stack operation is running")
					}
					if removeErr := os.Remove(path); removeErr == nil {
						return s.Lock()
					}
				}
			}
			return nil, errors.New("another gitea-stack operation is running")
		}
		return nil, err
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		file.Close()
		os.Remove(path)
		return nil, err
	}
	file.Close()
	return func() { _ = os.Remove(path) }, nil
}
