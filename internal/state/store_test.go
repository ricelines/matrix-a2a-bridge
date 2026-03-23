package state

import (
	"path/filepath"
	"testing"
)

func TestStorePersistsSessionAndCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if err := store.StoreSession(Session{
		HomeserverURL: "https://matrix.example.com",
		UserID:        "@bot:example.com",
		AccessToken:   "secret",
		DeviceID:      "DEVICE",
	}); err != nil {
		t.Fatalf("StoreSession() error = %v", err)
	}
	if err := store.SaveSyncCursor(SyncCursor{
		FilterID:  "filter",
		NextBatch: "batch",
	}); err != nil {
		t.Fatalf("SaveSyncCursor() error = %v", err)
	}
	if err := store.MarkHandled("$event"); err != nil {
		t.Fatalf("MarkHandled() error = %v", err)
	}

	reloaded, err := Open(path)
	if err != nil {
		t.Fatalf("Open(reload) error = %v", err)
	}

	snapshot := reloaded.Snapshot()
	if snapshot.Session.UserID != "@bot:example.com" {
		t.Fatalf("unexpected user id %q", snapshot.Session.UserID)
	}
	if snapshot.Sync.NextBatch != "batch" {
		t.Fatalf("unexpected next batch %q", snapshot.Sync.NextBatch)
	}
	if !reloaded.IsHandled("$event") {
		t.Fatal("expected handled event to persist")
	}
}

func TestStoreResetsSyncStateOnIdentityChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if err := store.StoreSession(Session{
		HomeserverURL: "https://matrix.example.com",
		UserID:        "@bot:example.com",
		AccessToken:   "secret",
	}); err != nil {
		t.Fatalf("StoreSession() error = %v", err)
	}
	if err := store.SaveSyncCursor(SyncCursor{
		FilterID:  "filter",
		NextBatch: "batch",
	}); err != nil {
		t.Fatalf("SaveSyncCursor() error = %v", err)
	}
	if err := store.MarkHandled("$event"); err != nil {
		t.Fatalf("MarkHandled() error = %v", err)
	}

	if err := store.StoreSession(Session{
		HomeserverURL: "https://matrix.example.com",
		UserID:        "@other:example.com",
		AccessToken:   "other",
	}); err != nil {
		t.Fatalf("StoreSession(identity change) error = %v", err)
	}

	snapshot := store.Snapshot()
	if snapshot.Sync.NextBatch != "" {
		t.Fatalf("expected next batch reset, got %q", snapshot.Sync.NextBatch)
	}
	if len(snapshot.HandledEventIDs) != 0 {
		t.Fatalf("expected handled event journal reset, got %d entries", len(snapshot.HandledEventIDs))
	}
}

func TestStorePersistsRoomSessionAtomicallyWithHandledEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	session := RoomSession{
		RoomID:       "!room:test",
		ContextID:    "ctx-1",
		LatestTaskID: "task-1",
	}
	if err := store.RecordRoomDelivery(session, "$event-1", "m.room.message"); err != nil {
		t.Fatalf("RecordRoomDelivery() error = %v", err)
	}

	reloaded, err := Open(path)
	if err != nil {
		t.Fatalf("Open(reload) error = %v", err)
	}

	got, ok := reloaded.RoomSession("!room:test")
	if !ok {
		t.Fatal("expected room session to persist")
	}
	if got.ContextID != "ctx-1" || got.LatestTaskID != "task-1" {
		t.Fatalf("room session = %#v, want context/task persisted", got)
	}
	if got.LastSuccessfulEventID != "$event-1" || got.LastSuccessfulEventType != "m.room.message" {
		t.Fatalf("room session = %#v, want last event metadata", got)
	}
	if !reloaded.IsHandled("$event-1") {
		t.Fatal("expected handled event to persist with room session update")
	}
}

func TestStoreResetsRoomSessionsOnIdentityChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if err := store.StoreSession(Session{
		HomeserverURL: "https://matrix.example.com",
		UserID:        "@bot:example.com",
		AccessToken:   "secret",
	}); err != nil {
		t.Fatalf("StoreSession() error = %v", err)
	}
	if err := store.RecordRoomDelivery(RoomSession{
		RoomID:       "!room:test",
		ContextID:    "ctx-1",
		LatestTaskID: "task-1",
	}, "$event-1", "m.room.message"); err != nil {
		t.Fatalf("RecordRoomDelivery() error = %v", err)
	}

	if err := store.StoreSession(Session{
		HomeserverURL: "https://matrix.example.com",
		UserID:        "@other:example.com",
		AccessToken:   "other",
	}); err != nil {
		t.Fatalf("StoreSession(identity change) error = %v", err)
	}

	if _, ok := store.RoomSession("!room:test"); ok {
		t.Fatal("expected room session catalog reset on identity change")
	}
}
