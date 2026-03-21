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
			sendCount++
			writeJSONRPCResponse(t, w, req.ID, map[string]any{
				"kind": "task",
				"id":   "task-123",
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
	matrixBridge.handleEvent(context.Background(), evt)

	if sendCount != 1 {
		t.Fatalf("message/send count = %d, want 1", sendCount)
	}
	if !store.IsHandled(evt.ID.String()) {
		t.Fatalf("expected event %s to be marked handled", evt.ID)
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
