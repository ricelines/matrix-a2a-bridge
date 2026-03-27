package bridge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	a2aproto "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"matrix-a2a-bridge/internal/a2a"
	"matrix-a2a-bridge/internal/config"
	"matrix-a2a-bridge/internal/state"
)

func TestLoginWithPasswordRetriesTransientFailure(t *testing.T) {
	var loginCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/login" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		loginCalls++
		if loginCalls < 3 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"try again"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"@bridge:example.com","access_token":"token","device_id":"DEV1"}`))
	}))
	defer server.Close()

	store, err := state.Open(filepath.Join(t.TempDir(), "bridge-state.json"))
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	client, err := mautrix.NewClient(server.URL, "", "")
	if err != nil {
		t.Fatalf("mautrix.NewClient() error = %v", err)
	}

	b := &Bridge{
		client: client,
		config: config.Config{
			HomeserverURL: server.URL,
			Username:      "bridge",
			Password:      "pass",
		},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		state: store,
	}
	if err := b.loginWithPassword(context.Background()); err != nil {
		t.Fatalf("loginWithPassword() error = %v", err)
	}
	if loginCalls != 3 {
		t.Fatalf("login call count = %d, want 3", loginCalls)
	}
	if snapshot := store.Snapshot(); snapshot.Session.AccessToken == "" || snapshot.Session.UserID == "" {
		t.Fatalf("expected persisted session after login, got %#v", snapshot.Session)
	}
}

func TestShouldForwardEvent(t *testing.T) {
	const self = id.UserID("@bridge:test")

	tests := []struct {
		name string
		evt  *event.Event
		want bool
	}{
		{
			name: "joined room timeline event from another user forwards",
			evt: &event.Event{
				Sender: id.UserID("@alice:test"),
				Type:   event.EventMessage,
				Mautrix: event.MautrixInfo{
					EventSource: event.SourceJoin | event.SourceTimeline,
				},
			},
			want: true,
		},
		{
			name: "invite state event forwards",
			evt: &event.Event{
				Sender: id.UserID("@alice:test"),
				Type:   event.StateMember,
				Mautrix: event.MautrixInfo{
					EventSource: event.SourceInvite | event.SourceState,
				},
			},
			want: true,
		},
		{
			name: "raw encrypted timeline event is ignored until decrypted",
			evt: &event.Event{
				Sender: id.UserID("@alice:test"),
				Type:   event.EventEncrypted,
				Mautrix: event.MautrixInfo{
					EventSource: event.SourceJoin | event.SourceTimeline,
				},
			},
			want: false,
		},
		{
			name: "decrypted timeline event forwards",
			evt: &event.Event{
				Sender: id.UserID("@alice:test"),
				Type:   event.EventMessage,
				Mautrix: event.MautrixInfo{
					EventSource: event.SourceJoin | event.SourceTimeline | event.SourceDecrypted,
				},
			},
			want: true,
		},
		{
			name: "self-authored event is ignored",
			evt: &event.Event{
				Sender: self,
				Type:   event.EventMessage,
				Mautrix: event.MautrixInfo{
					EventSource: event.SourceJoin | event.SourceTimeline,
				},
			},
			want: false,
		},
		{
			name: "ephemeral event is ignored",
			evt: &event.Event{
				Sender: id.UserID("@alice:test"),
				Type:   event.EphemeralEventReceipt,
				Mautrix: event.MautrixInfo{
					EventSource: event.SourceJoin | event.SourceEphemeral,
				},
			},
			want: false,
		},
		{
			name: "presence event is ignored",
			evt: &event.Event{
				Sender: id.UserID("@alice:test"),
				Type:   event.EphemeralEventPresence,
				Mautrix: event.MautrixInfo{
					EventSource: event.SourcePresence,
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldForwardEvent(self, tt.evt); got != tt.want {
				t.Fatalf("shouldForwardEvent() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestBuildRoomUpdateNotificationIncludesBatchEnvelope(t *testing.T) {
	self := id.UserID("@bridge:test")
	batch := roomUpdateBatch{
		RoomID: "!room:test",
		Updates: []roomUpdateSegment{{
			RoomSection: "invite",
			State: []*event.Event{{
				ID:       id.EventID("$evt:test"),
				RoomID:   id.RoomID("!room:test"),
				Sender:   id.UserID("@alice:test"),
				Type:     event.StateMember,
				StateKey: ptr("@bridge:test"),
				Content: event.Content{
					Raw: map[string]any{"membership": "invite"},
				},
				Mautrix: event.MautrixInfo{
					EventSource: event.SourceInvite | event.SourceState,
				},
			}},
		}},
	}

	notification, err := buildRoomUpdateNotification(self, "https://matrix.example.com", batch)
	if err != nil {
		t.Fatalf("buildRoomUpdateNotification() error = %v", err)
	}

	var body roomUpdateNotificationEnvelope
	if err := json.Unmarshal([]byte(notification.Body), &body); err != nil {
		t.Fatalf("Unmarshal(body) error = %v", err)
	}

	if body.Kind != "matrix_room_update" {
		t.Fatalf("body.kind = %q, want matrix_room_update", body.Kind)
	}
	if body.BridgeUserID != self.String() {
		t.Fatalf("body.bridge_user_id = %q, want %q", body.BridgeUserID, self.String())
	}
	if body.RoomID != "!room:test" {
		t.Fatalf("body.room_id = %q, want !room:test", body.RoomID)
	}
	if len(body.Updates) != 1 || body.Updates[0].RoomSection != "invite" || len(body.Updates[0].State) != 1 {
		t.Fatalf("body.updates = %#v, want one invite/state update", body.Updates)
	}

	metadata, ok := notification.Metadata["matrix_room_update"].(map[string]any)
	if !ok {
		t.Fatalf("notification metadata = %#v, want matrix_room_update envelope", notification.Metadata)
	}
	if metadata["kind"] != "matrix_room_update" {
		t.Fatalf("metadata.kind = %#v, want matrix_room_update", metadata["kind"])
	}
}

func TestHandleSyncResponseDeliversOnceAndMarksHandled(t *testing.T) {
	upstream := a2a.StartMockServer()
	defer upstream.Close()

	upstreamClient, err := a2a.New(context.Background(), upstream.BaseURL())
	if err != nil {
		t.Fatalf("a2a.New() error = %v", err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "bridge-state.json"))
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}

	matrixBridge := newTestBridge(t, store, upstreamClient)
	evt := messageEvent("$evt-1", "!room:test", "@alice:test", "hello")

	matrixBridge.handleSyncResponse(context.Background(), syncResponseForEvents(evt), "")
	waitForNotificationCount(t, upstream, 1)
	if !store.IsHandled(evt.ID.String()) {
		t.Fatalf("expected event %s to be marked handled", evt.ID)
	}

	matrixBridge.handleSyncResponse(context.Background(), syncResponseForEvents(evt), "")
	waitForStableNotificationCount(t, upstream, 1)

	notifications := upstream.Notifications()
	if len(notifications) != 1 {
		t.Fatalf("notification count = %d, want 1", len(notifications))
	}
}

func TestHandleSyncResponseBatchesRoomStateAndTimelineIntoOneNotification(t *testing.T) {
	upstream := a2a.StartMockServer()
	defer upstream.Close()

	upstreamClient, err := a2a.New(context.Background(), upstream.BaseURL())
	if err != nil {
		t.Fatalf("a2a.New() error = %v", err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "bridge-state.json"))
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}

	matrixBridge := newTestBridge(t, store, upstreamClient)
	stateEvt := memberInviteEvent("$invite-1", "!room:test", "@alice:test", "@bridge:test")
	messageEvt := messageEvent("$msg-1", "!room:test", "@alice:test", "hello")

	matrixBridge.handleSyncResponse(context.Background(), syncResponseForEvents(stateEvt, messageEvt), "")
	waitForNotificationCount(t, upstream, 1)

	notifications := upstream.Notifications()
	decoded := decodeRoomUpdateNotification(t, notifications[0].Body)
	if decoded.Kind != "matrix_room_update" || decoded.RoomID != "!room:test" {
		t.Fatalf("decoded notification = %#v, want room update for !room:test", decoded)
	}
	if len(decoded.Updates) != 1 {
		t.Fatalf("decoded.updates = %#v, want one room-scoped update", decoded.Updates)
	}
	if got := decoded.Updates[0]; got.RoomSection != "join" || len(got.State) != 1 || len(got.Timeline) != 1 {
		t.Fatalf("decoded update = %#v, want one join update with one state and one timeline event", got)
	}
}

func TestHandleSyncResponseMergesPendingUpdatesWhileTaskActive(t *testing.T) {
	type deliveredMessage struct {
		Body      string
		ContextID string
		TaskID    a2aproto.TaskID
	}

	var (
		delivered   []deliveredMessage
		allowFinish = make(chan struct{})
	)

	server := newLocalServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case a2asrv.WellKnownAgentCardPath:
			a2asrv.NewStaticAgentCardHandler(testAgentCard(serverBaseURL(r))).ServeHTTP(w, r)
			return
		case "/rpc":
		default:
			http.NotFound(w, r)
			return
		}

		req := decodeJSONRPCRequest(t, r)
		switch req.Method {
		case "message/send":
			var params a2aproto.MessageSendParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatalf("Unmarshal(message/send params) error = %v", err)
			}
			delivered = append(delivered, deliveredMessage{
				Body:      messageText(params.Message),
				ContextID: params.Message.ContextID,
				TaskID:    params.Message.TaskID,
			})

			writeJSONRPCResponse(t, w, req.ID, map[string]any{
				"kind":      "task",
				"id":        "task-1",
				"contextId": params.Message.ContextID,
				"status": map[string]any{
					"state": "submitted",
				},
			})
		case "tasks/get":
			<-allowFinish
			writeJSONRPCResponse(t, w, req.ID, map[string]any{
				"kind":      "task",
				"id":        "task-1",
				"contextId": delivered[0].ContextID,
				"status": map[string]any{
					"state": "input-required",
				},
			})
		default:
			t.Fatalf("unexpected JSON-RPC method %q", req.Method)
		}
	}))

	upstreamClient, err := a2a.New(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("a2a.New() error = %v", err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "bridge-state.json"))
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}

	matrixBridge := newTestBridge(t, store, upstreamClient)
	roomID := id.RoomID("!room:test")
	matrixBridge.handleSyncResponse(context.Background(), syncResponseForEvents(messageEvent("$evt-1", roomID.String(), "@alice:test", "first")), "")
	waitForLen(t, func() int { return len(delivered) }, 1, "first batch was not delivered")

	matrixBridge.handleSyncResponse(context.Background(), syncResponseForEvents(messageEvent("$evt-2", roomID.String(), "@alice:test", "second")), "")
	matrixBridge.handleSyncResponse(context.Background(), syncResponseForEvents(messageEvent("$evt-3", roomID.String(), "@alice:test", "third")), "")
	waitForStableLen(t, func() int { return len(delivered) }, 1)

	close(allowFinish)
	waitForLen(t, func() int { return len(delivered) }, 2, "merged pending batch was not delivered")

	second := decodeRoomUpdateNotification(t, delivered[1].Body)
	if len(second.Updates) != 2 {
		t.Fatalf("merged update count = %d, want 2", len(second.Updates))
	}
	if delivered[1].TaskID != "task-1" {
		t.Fatalf("second delivery task id = %q, want %q", delivered[1].TaskID, "task-1")
	}
	if got := roomMessageBodiesFromUpdate(second); !equalStrings(got, []string{"second", "third"}) {
		t.Fatalf("merged message bodies = %#v, want [second third]", got)
	}
}

func TestRunRoomQueueRetriesFailedDelivery(t *testing.T) {
	var sendCalls int

	server := newLocalServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case a2asrv.WellKnownAgentCardPath:
			a2asrv.NewStaticAgentCardHandler(testAgentCard(serverBaseURL(r))).ServeHTTP(w, r)
			return
		case "/rpc":
		default:
			http.NotFound(w, r)
			return
		}

		req := decodeJSONRPCRequest(t, r)
		switch req.Method {
		case "message/send":
			sendCalls++
			if sendCalls == 1 {
				http.Error(w, "temporary failure", http.StatusBadGateway)
				return
			}
			var params a2aproto.MessageSendParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatalf("Unmarshal(message/send params) error = %v", err)
			}
			writeJSONRPCResponse(t, w, req.ID, map[string]any{
				"kind":      "task",
				"id":        "task-retry",
				"contextId": params.Message.ContextID,
				"status": map[string]any{
					"state": "submitted",
				},
			})
		default:
			t.Fatalf("unexpected JSON-RPC method %q", req.Method)
		}
	}))

	upstreamClient, err := a2a.New(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("a2a.New() error = %v", err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "bridge-state.json"))
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}

	matrixBridge := newTestBridge(t, store, upstreamClient)
	evt := messageEvent("$evt-retry", "!room:test", "@alice:test", "retry me")
	matrixBridge.handleSyncResponse(context.Background(), syncResponseForEvents(evt), "")

	waitForLen(t, func() int { return sendCalls }, 2, "failed room delivery was not retried")
	if !store.IsHandled(evt.ID.String()) {
		t.Fatalf("expected retried event %s to be marked handled", evt.ID)
	}
}

func TestDeliverRoomUpdateContinuesSameRoomSession(t *testing.T) {
	type deliveredMessage struct {
		MessageID string
		ContextID string
		TaskID    a2aproto.TaskID
	}

	var delivered []deliveredMessage
	var getTaskCalls []string

	server := newLocalServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case a2asrv.WellKnownAgentCardPath:
			a2asrv.NewStaticAgentCardHandler(testAgentCard(serverBaseURL(r))).ServeHTTP(w, r)
			return
		case "/rpc":
		default:
			http.NotFound(w, r)
			return
		}

		req := decodeJSONRPCRequest(t, r)
		switch req.Method {
		case "message/send":
			var params a2aproto.MessageSendParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatalf("Unmarshal(message/send params) error = %v", err)
			}
			delivered = append(delivered, deliveredMessage{
				MessageID: params.Message.ID,
				ContextID: params.Message.ContextID,
				TaskID:    params.Message.TaskID,
			})

			writeJSONRPCResponse(t, w, req.ID, map[string]any{
				"kind":      "task",
				"id":        "task-1",
				"contextId": params.Message.ContextID,
				"status": map[string]any{
					"state": "submitted",
				},
			})
		case "tasks/get":
			var params a2aproto.TaskQueryParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatalf("Unmarshal(tasks/get params) error = %v", err)
			}
			getTaskCalls = append(getTaskCalls, string(params.ID))
			writeJSONRPCResponse(t, w, req.ID, map[string]any{
				"kind":      "task",
				"id":        string(params.ID),
				"contextId": delivered[0].ContextID,
				"status": map[string]any{
					"state": "input-required",
				},
			})
		default:
			t.Fatalf("unexpected JSON-RPC method %q", req.Method)
		}
	}))

	upstreamClient, err := a2a.New(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("a2a.New() error = %v", err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "bridge-state.json"))
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}

	matrixBridge := newTestBridge(t, store, upstreamClient)
	roomID := id.RoomID("!room:test")
	if err := matrixBridge.deliverRoomUpdate(context.Background(), roomUpdateForMessages(roomID, "first")); err != nil {
		t.Fatalf("deliverRoomUpdate(first) error = %v", err)
	}
	if err := matrixBridge.deliverRoomUpdate(context.Background(), roomUpdateForMessages(roomID, "second")); err != nil {
		t.Fatalf("deliverRoomUpdate(second) error = %v", err)
	}

	if len(delivered) != 2 {
		t.Fatalf("message/send count = %d, want 2", len(delivered))
	}
	if delivered[0].ContextID == "" {
		t.Fatal("first delivery context id should not be empty")
	}
	if delivered[0].TaskID != "" {
		t.Fatalf("first delivery task id = %q, want empty", delivered[0].TaskID)
	}
	if delivered[1].ContextID != delivered[0].ContextID {
		t.Fatalf("second delivery context id = %q, want %q", delivered[1].ContextID, delivered[0].ContextID)
	}
	if delivered[1].TaskID != "task-1" {
		t.Fatalf("second delivery task id = %q, want %q", delivered[1].TaskID, "task-1")
	}
	if len(getTaskCalls) != 1 || getTaskCalls[0] != "task-1" {
		t.Fatalf("tasks/get calls = %#v, want [task-1]", getTaskCalls)
	}

	session, ok := store.RoomSession(roomID.String())
	if !ok {
		t.Fatalf("expected stored room session for %s", roomID)
	}
	if session.ContextID != delivered[0].ContextID || session.LatestTaskID != "task-1" {
		t.Fatalf("stored room session = %#v, want updated continuation handle", session)
	}
}

func TestDeliverRoomUpdateRetriesWithoutReferenceTaskWhenLatestTaskIsMissing(t *testing.T) {
	type deliveredMessage struct {
		ContextID string
		TaskID    a2aproto.TaskID
	}

	var delivered []deliveredMessage

	server := newLocalServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case a2asrv.WellKnownAgentCardPath:
			a2asrv.NewStaticAgentCardHandler(testAgentCard(serverBaseURL(r))).ServeHTTP(w, r)
			return
		case "/rpc":
		default:
			http.NotFound(w, r)
			return
		}

		req := decodeJSONRPCRequest(t, r)
		switch req.Method {
		case "tasks/get":
			writeJSONRPCError(t, w, req.ID, -32001, "task not found", map[string]any{"error": a2aproto.ErrTaskNotFound.Error()})
		case "message/send":
			var params a2aproto.MessageSendParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatalf("Unmarshal(message/send params) error = %v", err)
			}
			delivered = append(delivered, deliveredMessage{
				ContextID: params.Message.ContextID,
				TaskID:    params.Message.TaskID,
			})
			writeJSONRPCResponse(t, w, req.ID, map[string]any{
				"kind":      "task",
				"id":        "task-new",
				"contextId": params.Message.ContextID,
				"status": map[string]any{
					"state": "submitted",
				},
			})
		default:
			t.Fatalf("unexpected JSON-RPC method %q", req.Method)
		}
	}))

	upstreamClient, err := a2a.New(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("a2a.New() error = %v", err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "bridge-state.json"))
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	if err := store.RecordRoomDelivery(state.RoomSession{
		RoomID:       "!room:test",
		ContextID:    "ctx-room",
		LatestTaskID: "task-missing",
	}, "$seed", "m.room.message"); err != nil {
		t.Fatalf("RecordRoomDelivery() error = %v", err)
	}

	matrixBridge := newTestBridge(t, store, upstreamClient)
	if err := matrixBridge.deliverRoomUpdate(context.Background(), roomUpdateForMessages(id.RoomID("!room:test"), "hello")); err != nil {
		t.Fatalf("deliverRoomUpdate() error = %v", err)
	}

	if len(delivered) != 1 {
		t.Fatalf("message/send count = %d, want 1", len(delivered))
	}
	if delivered[0].ContextID != "ctx-room" {
		t.Fatalf("delivery context id = %q, want %q", delivered[0].ContextID, "ctx-room")
	}
	if delivered[0].TaskID != "" {
		t.Fatalf("delivery task id = %q, want none after task lookup miss", delivered[0].TaskID)
	}
}

func TestDeliverRoomUpdateResetsContextAfterContinuationFailure(t *testing.T) {
	type deliveredMessage struct {
		ContextID string
		TaskID    a2aproto.TaskID
	}

	var delivered []deliveredMessage

	server := newLocalServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case a2asrv.WellKnownAgentCardPath:
			a2asrv.NewStaticAgentCardHandler(testAgentCard(serverBaseURL(r))).ServeHTTP(w, r)
			return
		case "/rpc":
		default:
			http.NotFound(w, r)
			return
		}

		req := decodeJSONRPCRequest(t, r)
		switch req.Method {
		case "tasks/get":
			writeJSONRPCResponse(t, w, req.ID, map[string]any{
				"kind":      "task",
				"id":        "task-old",
				"contextId": "ctx-room",
				"status": map[string]any{
					"state": "input-required",
				},
			})
		case "message/send":
			var params a2aproto.MessageSendParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatalf("Unmarshal(message/send params) error = %v", err)
			}
			delivered = append(delivered, deliveredMessage{
				ContextID: params.Message.ContextID,
				TaskID:    params.Message.TaskID,
			})

			if len(delivered) < 3 {
				writeJSONRPCResponse(t, w, req.ID, map[string]any{
					"kind":      "task",
					"id":        "task-failed",
					"contextId": params.Message.ContextID,
					"status": map[string]any{
						"state": "failed",
						"message": map[string]any{
							"kind":  "message",
							"role":  "agent",
							"parts": []map[string]any{{"kind": "text", "text": "message contextId different from task contextId"}},
						},
					},
				})
				return
			}

			writeJSONRPCResponse(t, w, req.ID, map[string]any{
				"kind":      "task",
				"id":        "task-reset",
				"contextId": params.Message.ContextID,
				"status": map[string]any{
					"state": "submitted",
				},
			})
		default:
			t.Fatalf("unexpected JSON-RPC method %q", req.Method)
		}
	}))

	upstreamClient, err := a2a.New(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("a2a.New() error = %v", err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "bridge-state.json"))
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	if err := store.RecordRoomDelivery(state.RoomSession{
		RoomID:       "!room:test",
		ContextID:    "ctx-room",
		LatestTaskID: "task-old",
	}, "$seed", "m.room.message"); err != nil {
		t.Fatalf("RecordRoomDelivery() error = %v", err)
	}

	matrixBridge := newTestBridge(t, store, upstreamClient)
	if err := matrixBridge.deliverRoomUpdate(context.Background(), roomUpdateForMessages(id.RoomID("!room:test"), "hello")); err != nil {
		t.Fatalf("deliverRoomUpdate() error = %v", err)
	}

	if len(delivered) != 3 {
		t.Fatalf("message/send count = %d, want 3", len(delivered))
	}
	if delivered[0].ContextID != "ctx-room" {
		t.Fatalf("first delivery context id = %q, want ctx-room", delivered[0].ContextID)
	}
	if delivered[0].TaskID != "task-old" {
		t.Fatalf("first delivery task id = %q, want %q", delivered[0].TaskID, "task-old")
	}
	if delivered[1].ContextID != "ctx-room" {
		t.Fatalf("second delivery context id = %q, want same context retry", delivered[1].ContextID)
	}
	if delivered[1].TaskID != "" {
		t.Fatalf("second delivery task id = %q, want none for same-context retry", delivered[1].TaskID)
	}
	if delivered[2].ContextID == "ctx-room" {
		t.Fatalf("third delivery context id = %q, want fresh context", delivered[2].ContextID)
	}
	if delivered[2].TaskID != "" {
		t.Fatalf("third delivery task id = %q, want none for fresh context retry", delivered[2].TaskID)
	}
}

func newTestBridge(t *testing.T, store *state.Store, upstreamClient *a2a.Client) *Bridge {
	t.Helper()

	client, err := mautrix.NewClient("https://matrix.example.com", id.UserID("@bridge:test"), "token")
	if err != nil {
		t.Fatalf("mautrix.NewClient() error = %v", err)
	}

	return &Bridge{
		client:   client,
		config:   config.Config{HomeserverURL: "https://matrix.example.com"},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		state:    store,
		upstream: upstreamClient,
		rooms:    make(map[string]*roomQueue),
		runCtx:   context.Background(),
	}
}

func messageEvent(eventID, roomID, sender, body string) *event.Event {
	return &event.Event{
		ID:     id.EventID(eventID),
		RoomID: id.RoomID(roomID),
		Sender: id.UserID(sender),
		Type:   event.EventMessage,
		Content: event.Content{
			Raw: map[string]any{
				"msgtype": "m.text",
				"body":    body,
			},
		},
		Mautrix: event.MautrixInfo{
			EventSource: event.SourceJoin | event.SourceTimeline,
		},
	}
}

func memberInviteEvent(eventID, roomID, sender, stateKey string) *event.Event {
	return &event.Event{
		ID:       id.EventID(eventID),
		RoomID:   id.RoomID(roomID),
		Sender:   id.UserID(sender),
		Type:     event.StateMember,
		StateKey: ptr(stateKey),
		Content: event.Content{
			Raw: map[string]any{"membership": "invite"},
		},
		Mautrix: event.MautrixInfo{
			EventSource: event.SourceJoin | event.SourceState,
		},
	}
}

func roomUpdateForMessages(roomID id.RoomID, bodies ...string) roomUpdateBatch {
	update := roomUpdateBatch{
		RoomID: roomID.String(),
		Updates: []roomUpdateSegment{{
			RoomSection: "join",
		}},
	}
	for _, body := range bodies {
		update.Updates[0].Timeline = append(update.Updates[0].Timeline, messageEvent(
			"$evt-"+strings.ReplaceAll(body, " ", "-"),
			roomID.String(),
			"@alice:test",
			body,
		))
	}
	return update
}

func syncResponseForEvents(events ...*event.Event) *mautrix.RespSync {
	resp := &mautrix.RespSync{
		Rooms: mautrix.RespSyncRooms{
			Join:   make(map[id.RoomID]*mautrix.SyncJoinedRoom),
			Invite: make(map[id.RoomID]*mautrix.SyncInvitedRoom),
			Leave:  make(map[id.RoomID]*mautrix.SyncLeftRoom),
		},
	}

	for _, evt := range events {
		if evt == nil {
			continue
		}
		roomSection, eventSection, ok := eventSourceSections(evt.Mautrix.EventSource)
		if !ok {
			continue
		}
		cloned := cloneEvent(evt)
		switch roomSection {
		case "join":
			room := resp.Rooms.Join[evt.RoomID]
			if room == nil {
				room = &mautrix.SyncJoinedRoom{}
				resp.Rooms.Join[evt.RoomID] = room
			}
			if eventSection == "state" {
				room.State.Events = append(room.State.Events, cloned)
			} else {
				room.Timeline.Events = append(room.Timeline.Events, cloned)
			}
		case "invite":
			room := resp.Rooms.Invite[evt.RoomID]
			if room == nil {
				room = &mautrix.SyncInvitedRoom{}
				resp.Rooms.Invite[evt.RoomID] = room
			}
			room.State.Events = append(room.State.Events, cloned)
		case "leave":
			room := resp.Rooms.Leave[evt.RoomID]
			if room == nil {
				room = &mautrix.SyncLeftRoom{}
				resp.Rooms.Leave[evt.RoomID] = room
			}
			if eventSection == "state" {
				room.State.Events = append(room.State.Events, cloned)
			} else {
				room.Timeline.Events = append(room.Timeline.Events, cloned)
			}
		}
	}

	return resp
}

func waitForNotificationCount(t *testing.T, upstream *a2a.MockServer, want int) {
	t.Helper()
	waitForLen(t, upstream.NotificationCount, want, "notification count did not reach target")
}

func waitForStableNotificationCount(t *testing.T, upstream *a2a.MockServer, want int) {
	t.Helper()
	waitForStableLen(t, upstream.NotificationCount, want)
}

func waitForLen(t *testing.T, count func() int, want int, message string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if count() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: got %d, want at least %d", message, count(), want)
}

func waitForStableLen(t *testing.T, count func() int, want int) {
	t.Helper()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if count() != want {
			t.Fatalf("count = %d, want stable %d", count(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func decodeRoomUpdateNotification(t *testing.T, body string) roomUpdateNotificationEnvelope {
	t.Helper()

	var notification roomUpdateNotificationEnvelope
	if err := json.Unmarshal([]byte(body), &notification); err != nil {
		t.Fatalf("json.Unmarshal(notification) error = %v", err)
	}
	return notification
}

func roomMessageBodiesFromUpdate(update roomUpdateNotificationEnvelope) []string {
	var bodies []string
	for _, segment := range update.Updates {
		for _, evt := range segment.Timeline {
			if evt == nil || evt.Type != event.EventMessage {
				continue
			}
			if body, _ := evt.Content.Raw["body"].(string); body != "" {
				bodies = append(bodies, body)
			}
		}
	}
	return bodies
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for idx := range left {
		if left[idx] != right[idx] {
			return false
		}
	}
	return true
}

func ptr[T any](value T) *T {
	return &value
}

func messageText(msg *a2aproto.Message) string {
	if msg == nil {
		return ""
	}
	for _, part := range msg.Parts {
		switch typed := part.(type) {
		case a2aproto.TextPart:
			return typed.Text
		case *a2aproto.TextPart:
			return typed.Text
		}
	}
	return ""
}

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      any             `json:"id"`
}

func decodeJSONRPCRequest(t *testing.T, r *http.Request) jsonrpcRequest {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Fatalf("HTTP method = %s, want POST", r.Method)
	}

	var req jsonrpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("Decode JSON-RPC request error = %v", err)
	}
	return req
}

func writeJSONRPCResponse(t *testing.T, w http.ResponseWriter, id any, result any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}); err != nil {
		t.Fatalf("Encode JSON-RPC response error = %v", err)
	}
}

func writeJSONRPCError(t *testing.T, w http.ResponseWriter, id any, code int, message string, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
			"data":    data,
		},
	}); err != nil {
		t.Fatalf("Encode JSON-RPC error response error = %v", err)
	}
}

func testAgentCard(baseURL string) *a2aproto.AgentCard {
	return &a2aproto.AgentCard{
		Name:               "Test Agent",
		Description:        "Test upstream A2A endpoint for Matrix A2A bridge regression coverage.",
		Version:            "test",
		URL:                baseURL + "/rpc",
		ProtocolVersion:    string(a2aproto.Version),
		PreferredTransport: a2aproto.TransportProtocolJSONRPC,
		Capabilities:       a2aproto.AgentCapabilities{Streaming: false},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		AdditionalInterfaces: []a2aproto.AgentInterface{{
			Transport: a2aproto.TransportProtocolJSONRPC,
			URL:       baseURL + "/rpc",
		}},
	}
}

func serverBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func newLocalServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "permission denied") {
			t.Skipf("local listener unavailable in sandbox: %v", err)
		}
		t.Fatalf("Listen() error = %v", err)
	}
	server := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: handler},
	}
	server.Start()
	t.Cleanup(server.Close)
	return server
}
