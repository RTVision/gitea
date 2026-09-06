// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package localstate

import (
	"crypto/rand"
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

type ForeignLockError struct {
	Host string
	PID  int
	Path string
}

func (e ForeignLockError) Error() string {
	return fmt.Sprintf("repository is locked by pid %d on host %s.\nFinish or stop that command on %s, then remove %s there.", e.PID, e.Host, e.Host, e.Path)
}

type lockOwner struct {
	Host  string `json:"host"`
	PID   int    `json:"pid"`
	Nonce string `json:"nonce,omitempty"`
}

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
	host, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		owner := lockOwner{Host: host, PID: os.Getpid(), Nonce: rand.Text()}
		data, marshalErr := json.Marshal(owner)
		if marshalErr == nil {
			_, marshalErr = file.Write(append(data, '\n'))
		}
		closeErr := file.Close()
		if marshalErr != nil || closeErr != nil {
			_ = os.Remove(path)
			return nil, errors.Join(marshalErr, closeErr)
		}
		return func() {
			var current lockOwner
			currentData, err := os.ReadFile(path)
			if err == nil && json.Unmarshal(currentData, &current) == nil && current == owner {
				_ = os.Remove(path)
			}
		}, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return nil, errors.New("another gitea-stack operation is running")
	}
	var owner lockOwner
	if unmarshalErr := json.Unmarshal(data, &owner); unmarshalErr != nil || owner.Host == "" || owner.PID <= 0 {
		if _, legacyErr := strconv.Atoi(strings.TrimSpace(string(data))); legacyErr == nil {
			return nil, fmt.Errorf("another gitea-stack operation may be running; remove legacy lock %s only after checking every host", path)
		}
		return nil, errors.New("another gitea-stack operation is running")
	}
	if owner.Host != host {
		return nil, ForeignLockError{Host: owner.Host, PID: owner.PID, Path: path}
	}
	process, findErr := os.FindProcess(owner.PID)
	if findErr != nil {
		return nil, errors.New("another gitea-stack operation is running")
	}
	signalErr := process.Signal(syscall.Signal(0))
	if signalErr == nil || (!errors.Is(signalErr, os.ErrProcessDone) && !errors.Is(signalErr, syscall.ESRCH)) {
		return nil, errors.New("another gitea-stack operation is running")
	}
	return nil, fmt.Errorf("repository lock belongs to stopped pid %d on this host; remove %s and retry", owner.PID, path)
}
