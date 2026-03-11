package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const handledEventLimit = 4096

type Session struct {
	HomeserverURL string `json:"homeserver_url,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	DeviceID      string `json:"device_id,omitempty"`
}

type SyncCursor struct {
	FilterID  string `json:"filter_id,omitempty"`
	NextBatch string `json:"next_batch,omitempty"`
}

type FileState struct {
	Session         Session    `json:"session"`
	Sync            SyncCursor `json:"sync"`
	HandledEventIDs []string   `json:"handled_event_ids,omitempty"`
}

type Store struct {
	path    string
	mu      sync.Mutex
	state   FileState
	handled map[string]struct{}
}

func Open(path string) (*Store, error) {
	store := &Store{
		path:    path,
		state:   FileState{},
		handled: make(map[string]struct{}),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return nil, fmt.Errorf("read state file: %w", err)
	}
	if err := json.Unmarshal(data, &store.state); err != nil {
		return nil, fmt.Errorf("decode state file: %w", err)
	}
	store.rebuildHandledIndex()
	return store, nil
}

func (s *Store) Snapshot() FileState {
	s.mu.Lock()
	defer s.mu.Unlock()

	copyState := s.state
	copyState.HandledEventIDs = append([]string(nil), s.state.HandledEventIDs...)
	return copyState
}

func (s *Store) StoreSession(session Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.Session.HomeserverURL != "" &&
		(s.state.Session.HomeserverURL != session.HomeserverURL || s.state.Session.UserID != session.UserID) {
		s.state.Sync = SyncCursor{}
		s.state.HandledEventIDs = nil
		s.handled = make(map[string]struct{})
	}

	s.state.Session = session
	return s.saveLocked()
}

func (s *Store) SaveSyncCursor(cursor SyncCursor) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state.Sync = cursor
	return s.saveLocked()
}

func (s *Store) MarkHandled(eventID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.handled[eventID]; ok {
		return nil
	}

	s.state.HandledEventIDs = append(s.state.HandledEventIDs, eventID)
	s.handled[eventID] = struct{}{}
	if len(s.state.HandledEventIDs) > handledEventLimit {
		evicted := s.state.HandledEventIDs[0]
		s.state.HandledEventIDs = s.state.HandledEventIDs[1:]
		delete(s.handled, evicted)
	}
	return s.saveLocked()
}

func (s *Store) IsHandled(eventID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.handled[eventID]
	return ok
}

func (s *Store) rebuildHandledIndex() {
	s.handled = make(map[string]struct{}, len(s.state.HandledEventIDs))
	if len(s.state.HandledEventIDs) > handledEventLimit {
		s.state.HandledEventIDs = append([]string(nil), s.state.HandledEventIDs[len(s.state.HandledEventIDs)-handledEventLimit:]...)
	}
	for _, eventID := range s.state.HandledEventIDs {
		s.handled[eventID] = struct{}{}
	}
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}

	tmpPath := s.path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open temp state file: %w", err)
	}

	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync temp state file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}

	return nil
}
