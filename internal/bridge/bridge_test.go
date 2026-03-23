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
	"sync"
	"testing"

	a2aproto "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"matrix-a2a-bridge/internal/a2a"
	"matrix-a2a-bridge/internal/state"
)

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

func TestBuildEventNotificationIncludesEventEnvelope(t *testing.T) {
	self := id.UserID("@bridge:test")
	roomID := id.RoomID("!room:test")
	eventID := id.EventID("$evt:test")
	stateKey := "@bridge:test"

	evt := &event.Event{
		StateKey: &stateKey,
		Sender:   id.UserID("@alice:test"),
		Type:     event.StateMember,
		ID:       eventID,
		RoomID:   roomID,
		Content: event.Content{
			Raw: map[string]any{
				"membership": "invite",
			},
		},
		Mautrix: event.MautrixInfo{
			EventSource: event.SourceInvite | event.SourceState,
		},
	}

	notification, err := buildEventNotification(self, "https://matrix.example.com", evt)
	if err != nil {
		t.Fatalf("buildEventNotification() error = %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(notification.Body), &body); err != nil {
		t.Fatalf("Unmarshal(body) error = %v", err)
	}

	if body["kind"] != "matrix_event" {
		t.Fatalf("body.kind = %#v, want matrix_event", body["kind"])
	}
	if body["bridge_user_id"] != self.String() {
		t.Fatalf("body.bridge_user_id = %#v, want %q", body["bridge_user_id"], self.String())
	}
	source, ok := body["source"].(map[string]any)
	if !ok {
		t.Fatalf("body.source = %#v, want object", body["source"])
	}
	if source["room_section"] != "invite" || source["event_section"] != "state" {
		t.Fatalf("body.source = %#v, want invite/state", source)
	}
	eventBody, ok := body["event"].(map[string]any)
	if !ok {
		t.Fatalf("body.event = %#v, want object", body["event"])
	}
	if eventBody["event_id"] != eventID.String() || eventBody["room_id"] != roomID.String() {
		t.Fatalf("body.event = %#v, want event and room IDs", eventBody)
	}

	metadata, ok := notification.Metadata["matrix_event"].(map[string]any)
	if !ok {
		t.Fatalf("notification metadata = %#v, want matrix_event envelope", notification.Metadata)
	}
	if metadata["kind"] != "matrix_event" {
		t.Fatalf("metadata.kind = %#v, want matrix_event", metadata["kind"])
	}
}

func TestHandleEventDeliversOnceAndMarksHandled(t *testing.T) {
	sendCount := 0

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
			sendCount++
			writeJSONRPCResponse(t, w, req.ID, map[string]any{
				"kind":      "task",
				"id":        "task-123",
				"contextId": params.Message.ContextID,
				"status": map[string]any{
					"state": "completed",
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

	client, err := mautrix.NewClient("https://matrix.example.com", id.UserID("@bridge:test"), "token")
	if err != nil {
		t.Fatalf("mautrix.NewClient() error = %v", err)
	}

	matrixBridge := &Bridge{
		client:   client,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		state:    store,
		upstream: upstreamClient,
	}

	evt := &event.Event{
		ID:     id.EventID("$evt-1"),
		RoomID: id.RoomID("!room:test"),
		Sender: id.UserID("@alice:test"),
		Type:   event.EventMessage,
		Content: event.Content{
			Raw: map[string]any{
				"msgtype": "m.text",
				"body":    "hello",
			},
		},
		Mautrix: event.MautrixInfo{
			EventSource: event.SourceJoin | event.SourceTimeline,
		},
	}

	matrixBridge.handleEvent(context.Background(), evt)
	if !store.IsHandled(evt.ID.String()) {
		t.Fatalf("expected event %s to be marked handled after first delivery", evt.ID)
	}
	matrixBridge.handleEvent(context.Background(), evt)

	if sendCount != 1 {
		t.Fatalf("message/send count = %d, want 1", sendCount)
	}
	if !store.IsHandled(evt.ID.String()) {
		t.Fatalf("expected event %s to be marked handled", evt.ID)
	}
}

func TestHandleEventContinuesSameRoomSession(t *testing.T) {
	type deliveredMessage struct {
		MessageID      string
		ContextID      string
		ReferenceTasks []a2aproto.TaskID
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
				MessageID:      params.Message.ID,
				ContextID:      params.Message.ContextID,
				ReferenceTasks: append([]a2aproto.TaskID(nil), params.Message.ReferenceTasks...),
			})

			taskID := "task-1"
			if len(delivered) == 2 {
				taskID = "task-2"
			}
			writeJSONRPCResponse(t, w, req.ID, map[string]any{
				"kind":      "task",
				"id":        taskID,
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
					"state": "completed",
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

	client, err := mautrix.NewClient("https://matrix.example.com", id.UserID("@bridge:test"), "token")
	if err != nil {
		t.Fatalf("mautrix.NewClient() error = %v", err)
	}

	matrixBridge := &Bridge{
		client:    client,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		state:     store,
		upstream:  upstreamClient,
		roomLocks: make(map[string]*sync.Mutex),
	}

	roomID := id.RoomID("!room:test")
	matrixBridge.handleEvent(context.Background(), &event.Event{
		ID:     id.EventID("$evt-1"),
		RoomID: roomID,
		Sender: id.UserID("@alice:test"),
		Type:   event.EventMessage,
		Content: event.Content{Raw: map[string]any{
			"msgtype": "m.text",
			"body":    "first",
		}},
		Mautrix: event.MautrixInfo{EventSource: event.SourceJoin | event.SourceTimeline},
	})
	matrixBridge.handleEvent(context.Background(), &event.Event{
		ID:     id.EventID("$evt-2"),
		RoomID: roomID,
		Sender: id.UserID("@alice:test"),
		Type:   event.EventMessage,
		Content: event.Content{Raw: map[string]any{
			"msgtype": "m.text",
			"body":    "second",
		}},
		Mautrix: event.MautrixInfo{EventSource: event.SourceJoin | event.SourceTimeline},
	})

	if len(delivered) != 2 {
		t.Fatalf("message/send count = %d, want 2", len(delivered))
	}
	if delivered[0].ContextID == "" {
		t.Fatal("first delivery context id should not be empty")
	}
	if len(delivered[0].ReferenceTasks) != 0 {
		t.Fatalf("first delivery reference tasks = %#v, want none", delivered[0].ReferenceTasks)
	}
	if delivered[1].ContextID != delivered[0].ContextID {
		t.Fatalf("second delivery context id = %q, want %q", delivered[1].ContextID, delivered[0].ContextID)
	}
	if len(delivered[1].ReferenceTasks) != 1 || delivered[1].ReferenceTasks[0] != "task-1" {
		t.Fatalf("second delivery reference tasks = %#v, want [task-1]", delivered[1].ReferenceTasks)
	}
	if len(getTaskCalls) != 1 || getTaskCalls[0] != "task-1" {
		t.Fatalf("tasks/get calls = %#v, want [task-1]", getTaskCalls)
	}

	session, ok := store.RoomSession(roomID.String())
	if !ok {
		t.Fatalf("expected stored room session for %s", roomID)
	}
	if session.ContextID != delivered[0].ContextID || session.LatestTaskID != "task-2" {
		t.Fatalf("stored room session = %#v, want updated continuation handle", session)
	}
}

func TestHandleEventRetriesWithoutReferenceTaskWhenLatestTaskIsMissing(t *testing.T) {
	type deliveredMessage struct {
		ContextID      string
		ReferenceTasks []a2aproto.TaskID
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
				ContextID:      params.Message.ContextID,
				ReferenceTasks: append([]a2aproto.TaskID(nil), params.Message.ReferenceTasks...),
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

	client, err := mautrix.NewClient("https://matrix.example.com", id.UserID("@bridge:test"), "token")
	if err != nil {
		t.Fatalf("mautrix.NewClient() error = %v", err)
	}

	matrixBridge := &Bridge{
		client:    client,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		state:     store,
		upstream:  upstreamClient,
		roomLocks: make(map[string]*sync.Mutex),
	}

	matrixBridge.handleEvent(context.Background(), &event.Event{
		ID:     id.EventID("$evt-1"),
		RoomID: id.RoomID("!room:test"),
		Sender: id.UserID("@alice:test"),
		Type:   event.EventMessage,
		Content: event.Content{Raw: map[string]any{
			"msgtype": "m.text",
			"body":    "hello",
		}},
		Mautrix: event.MautrixInfo{EventSource: event.SourceJoin | event.SourceTimeline},
	})

	if len(delivered) != 1 {
		t.Fatalf("message/send count = %d, want 1", len(delivered))
	}
	if delivered[0].ContextID != "ctx-room" {
		t.Fatalf("delivery context id = %q, want %q", delivered[0].ContextID, "ctx-room")
	}
	if len(delivered[0].ReferenceTasks) != 0 {
		t.Fatalf("delivery reference tasks = %#v, want none after task lookup miss", delivered[0].ReferenceTasks)
	}
}

func TestHandleEventResetsContextAfterContinuationFailure(t *testing.T) {
	type deliveredMessage struct {
		ContextID      string
		ReferenceTasks []a2aproto.TaskID
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
					"state": "completed",
				},
			})
		case "message/send":
			var params a2aproto.MessageSendParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatalf("Unmarshal(message/send params) error = %v", err)
			}
			delivered = append(delivered, deliveredMessage{
				ContextID:      params.Message.ContextID,
				ReferenceTasks: append([]a2aproto.TaskID(nil), params.Message.ReferenceTasks...),
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

	client, err := mautrix.NewClient("https://matrix.example.com", id.UserID("@bridge:test"), "token")
	if err != nil {
		t.Fatalf("mautrix.NewClient() error = %v", err)
	}

	matrixBridge := &Bridge{
		client:    client,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		state:     store,
		upstream:  upstreamClient,
		roomLocks: make(map[string]*sync.Mutex),
	}

	matrixBridge.handleEvent(context.Background(), &event.Event{
		ID:     id.EventID("$evt-1"),
		RoomID: id.RoomID("!room:test"),
		Sender: id.UserID("@alice:test"),
		Type:   event.EventMessage,
		Content: event.Content{Raw: map[string]any{
			"msgtype": "m.text",
			"body":    "hello",
		}},
		Mautrix: event.MautrixInfo{EventSource: event.SourceJoin | event.SourceTimeline},
	})

	if len(delivered) != 3 {
		t.Fatalf("message/send count = %d, want 3", len(delivered))
	}
	if delivered[0].ContextID != "ctx-room" {
		t.Fatalf("first delivery context id = %q, want ctx-room", delivered[0].ContextID)
	}
	if len(delivered[0].ReferenceTasks) != 1 || delivered[0].ReferenceTasks[0] != "task-old" {
		t.Fatalf("first delivery reference tasks = %#v, want [task-old]", delivered[0].ReferenceTasks)
	}
	if delivered[1].ContextID != "ctx-room" {
		t.Fatalf("second delivery context id = %q, want same context retry", delivered[1].ContextID)
	}
	if len(delivered[1].ReferenceTasks) != 0 {
		t.Fatalf("second delivery reference tasks = %#v, want none for same-context retry", delivered[1].ReferenceTasks)
	}
	if delivered[2].ContextID == "ctx-room" {
		t.Fatalf("third delivery context id = %q, want fresh context", delivered[2].ContextID)
	}
	if len(delivered[2].ReferenceTasks) != 0 {
		t.Fatalf("third delivery reference tasks = %#v, want none for fresh context retry", delivered[2].ReferenceTasks)
	}
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
