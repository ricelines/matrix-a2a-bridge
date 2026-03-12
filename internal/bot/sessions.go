package bot

import (
	"sync"
	"time"

	"maunium.net/go/mautrix/id"
)

type session struct {
	RoomID       id.RoomID
	UserID       id.UserID
	TaskID       string
	ContextID    string
	LastActivity time.Time
}

type sessionManager struct {
	idleTimeout time.Duration
	mu          sync.Mutex
	sessions    map[id.RoomID]session
}

func newSessionManager(idleTimeout time.Duration) *sessionManager {
	return &sessionManager{
		idleTimeout: idleTimeout,
		sessions:    make(map[id.RoomID]session),
	}
}

func (m *sessionManager) Active(roomID id.RoomID, now time.Time) (session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, ok := m.sessions[roomID]
	if !ok || m.expired(current, now) {
		return session{}, false
	}
	return current, true
}

func (m *sessionManager) ExpireRoom(roomID id.RoomID, now time.Time) (session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, ok := m.sessions[roomID]
	if !ok || !m.expired(current, now) {
		return session{}, false
	}

	delete(m.sessions, roomID)
	return current, true
}

func (m *sessionManager) Expired(now time.Time) []session {
	m.mu.Lock()
	defer m.mu.Unlock()

	var expired []session
	for roomID, current := range m.sessions {
		if !m.expired(current, now) {
			continue
		}
		expired = append(expired, current)
		delete(m.sessions, roomID)
	}
	return expired
}

func (m *sessionManager) Put(current session) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sessions[current.RoomID] = current
}

func (m *sessionManager) Remove(roomID id.RoomID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.sessions, roomID)
}

func (m *sessionManager) expired(current session, now time.Time) bool {
	return m.idleTimeout > 0 && now.Sub(current.LastActivity) >= m.idleTimeout
}
