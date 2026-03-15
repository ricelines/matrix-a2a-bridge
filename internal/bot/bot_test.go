package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	a2aproto "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"matrix-a2a-bridge/internal/a2a"
	"matrix-a2a-bridge/internal/state"
)

const testAgentEndpointPath = "/rpc"

func TestPersistReplyStateStoresOnlyInterruptibleTasks(t *testing.T) {
	roomID := id.RoomID("!room:test")
	userID := id.UserID("@alice:test")
	now := time.Now().UTC()

	t.Run("working task is not cached for reuse", func(t *testing.T) {
		bot := &Bot{sessions: newSessionManager(time.Minute)}
		err := bot.persistReplyState(context.Background(), roomID, userID, userRecord{ContextID: "ctx"}, a2a.Response{
			TaskID:    "task-working",
			ContextID: "ctx",
			State:     a2aproto.TaskStateWorking,
		})
		if err != nil {
			t.Fatalf("persistReplyState() error = %v", err)
		}

		if _, ok := bot.sessions.Active(roomID, now); ok {
			t.Fatal("working task unexpectedly remained active for same-task continuation")
		}
	})

	t.Run("input required task remains cached", func(t *testing.T) {
		bot := &Bot{sessions: newSessionManager(time.Minute)}
		err := bot.persistReplyState(context.Background(), roomID, userID, userRecord{ContextID: "ctx"}, a2a.Response{
			TaskID:    "task-input",
			ContextID: "ctx",
			State:     a2aproto.TaskStateInputRequired,
		})
		if err != nil {
			t.Fatalf("persistReplyState() error = %v", err)
		}

		current, ok := bot.sessions.Active(roomID, now)
		if !ok {
			t.Fatal("input-required task was not retained")
		}
		if current.TaskID != "task-input" || current.ContextID != "ctx" {
			t.Fatalf("active session = %+v, want task-input/ctx", current)
		}
	})
}

func TestHandleMessageEventWaitsForUsableReplyAndDoesNotReuseCompletedTask(t *testing.T) {
	const (
		roomID      = id.RoomID("!room:test")
		userID      = id.UserID("@alice:test")
		botUserID   = id.UserID("@bot:test")
		firstTaskID = "task-1"
		secondTask  = "task-2"
		contextID   = "ctx-1"
	)

	type sentA2AMessage struct {
		TaskID    string
		ContextID string
		Text      string
		Metadata  map[string]any
	}

	var (
		a2aMu        sync.Mutex
		a2aMessages  []sentA2AMessage
		getTaskCalls int
	)

	a2aServer := newLocalServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case a2asrv.WellKnownAgentCardPath:
			a2asrv.NewStaticAgentCardHandler(testAgentCard(serverBaseURL(r))).ServeHTTP(w, r)
			return
		case testAgentEndpointPath:
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

			msg := params.Message
			if msg == nil {
				t.Fatal("message/send did not include a message")
			}

			sent := sentA2AMessage{
				TaskID:    string(msg.TaskID),
				ContextID: msg.ContextID,
				Text:      currentUserText(msg),
				Metadata:  params.Metadata,
			}

			a2aMu.Lock()
			a2aMessages = append(a2aMessages, sent)
			callNumber := len(a2aMessages)
			a2aMu.Unlock()

			switch callNumber {
			case 1:
				writeJSONRPCResponse(t, w, req.ID, map[string]any{
					"kind":      "task",
					"id":        firstTaskID,
					"contextId": contextID,
					"status": map[string]any{
						"state": "working",
					},
				})
			case 2:
				writeJSONRPCResponse(t, w, req.ID, map[string]any{
					"kind":      "task",
					"id":        secondTask,
					"contextId": contextID,
					"status": map[string]any{
						"state": "completed",
						"message": map[string]any{
							"kind":      "message",
							"messageId": "msg-2",
							"role":      "agent",
							"parts": []map[string]any{{
								"kind": "text",
								"text": "Second reply",
							}},
						},
					},
				})
			default:
				t.Fatalf("unexpected message/send call %d", callNumber)
			}
		case "tasks/get":
			a2aMu.Lock()
			getTaskCalls++
			callNumber := getTaskCalls
			a2aMu.Unlock()

			result := map[string]any{
				"kind":      "task",
				"id":        firstTaskID,
				"contextId": contextID,
				"status": map[string]any{
					"state": "working",
				},
			}
			if callNumber >= 2 {
				result["status"] = map[string]any{
					"state": "completed",
					"message": map[string]any{
						"kind":      "message",
						"messageId": "msg-1",
						"role":      "agent",
						"parts": []map[string]any{{
							"kind": "text",
							"text": "First reply",
						}},
					},
				}
			}
			writeJSONRPCResponse(t, w, req.ID, result)
		default:
			t.Fatalf("unexpected JSON-RPC method %q", req.Method)
		}
	}))

	matrixServer := newMatrixTestServer(t)

	client, err := mautrix.NewClient(matrixServer.URL(), botUserID, "token")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	upstreamClient, err := a2a.New(context.Background(), a2aServer.URL)
	if err != nil {
		t.Fatalf("a2a.New() error = %v", err)
	}

	stateStore, err := state.Open(path.Join(t.TempDir(), "bot-state.json"))
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}

	bot := &Bot{
		client:   client,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		state:    stateStore,
		upstream: upstreamClient,
		users:    newUserDirectory(client),
		sessions: newSessionManager(time.Minute),
		roomPeers: map[id.RoomID]id.UserID{
			roomID: userID,
		},
	}

	firstEvent := &event.Event{
		ID:     id.EventID("$evt-1"),
		RoomID: roomID,
		Sender: userID,
		Type:   event.EventMessage,
		Content: event.Content{
			Parsed: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    "hello",
			},
		},
	}
	bot.handleMessageEvent(context.Background(), firstEvent)

	if got := matrixServer.MessageBodies(); len(got) != 1 || got[0] != "First reply" {
		t.Fatalf("first bot reply bodies = %v, want [First reply]", got)
	}

	record, err := bot.users.Load(context.Background(), userID)
	if err != nil {
		t.Fatalf("users.Load() error = %v", err)
	}
	if record.ContextID != contextID {
		t.Fatalf("record.ContextID = %q, want %q", record.ContextID, contextID)
	}
	if _, ok := bot.sessions.Active(roomID, time.Now().UTC()); ok {
		t.Fatal("completed task unexpectedly remained active")
	}

	secondEvent := &event.Event{
		ID:     id.EventID("$evt-2"),
		RoomID: roomID,
		Sender: userID,
		Type:   event.EventMessage,
		Content: event.Content{
			Parsed: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    "status please",
			},
		},
	}
	bot.handleMessageEvent(context.Background(), secondEvent)

	if got := matrixServer.MessageBodies(); len(got) != 2 || got[1] != "Second reply" {
		t.Fatalf("all bot reply bodies = %v, want [First reply Second reply]", got)
	}

	a2aMu.Lock()
	defer a2aMu.Unlock()

	if len(a2aMessages) != 2 {
		t.Fatalf("message/send call count = %d, want 2", len(a2aMessages))
	}
	if a2aMessages[0].TaskID != "" || a2aMessages[0].ContextID != "" {
		t.Fatalf("first A2A message = %+v, want new task without task/context IDs", a2aMessages[0])
	}
	if a2aMessages[0].Text != "hello" {
		t.Fatalf("first A2A message.Text = %q, want %q", a2aMessages[0].Text, "hello")
	}
	if _, ok := a2aMessages[0].Metadata["workflow"]; ok {
		t.Fatalf("first A2A message metadata unexpectedly included workflow data: %+v", a2aMessages[0].Metadata)
	}
	matrixMetadata, ok := a2aMessages[0].Metadata["matrix"].(map[string]any)
	if !ok {
		t.Fatalf("first A2A message metadata = %+v, want matrix metadata", a2aMessages[0].Metadata)
	}
	if matrixMetadata["room_id"] != roomID.String() || matrixMetadata["user_id"] != userID.String() {
		t.Fatalf("first A2A message matrix metadata = %+v, want room/user IDs", matrixMetadata)
	}
	if a2aMessages[1].TaskID != "" {
		t.Fatalf("second A2A message.TaskID = %q, want empty after completed first task", a2aMessages[1].TaskID)
	}
	if a2aMessages[1].ContextID != contextID {
		t.Fatalf("second A2A message.ContextID = %q, want %q", a2aMessages[1].ContextID, contextID)
	}
	if a2aMessages[1].Text != "status please" {
		t.Fatalf("second A2A message.Text = %q, want %q", a2aMessages[1].Text, "status please")
	}
	if getTaskCalls < 2 {
		t.Fatalf("tasks/get call count = %d, want at least 2 polls before completion", getTaskCalls)
	}
}

type matrixTestServer struct {
	t           *testing.T
	server      *httptest.Server
	mu          sync.Mutex
	accountData map[string]json.RawMessage
	messages    []string
}

func newMatrixTestServer(t *testing.T) *matrixTestServer {
	t.Helper()

	server := &matrixTestServer{
		t:           t,
		accountData: make(map[string]json.RawMessage),
	}
	server.server = newLocalServer(t, http.HandlerFunc(server.handle))
	return server
}

func (s *matrixTestServer) URL() string {
	return s.server.URL
}

func (s *matrixTestServer) Close() {
	s.server.Close()
}

func (s *matrixTestServer) MessageBodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.messages...)
}

func (s *matrixTestServer) handle(w http.ResponseWriter, r *http.Request) {
	s.t.Helper()

	switch {
	case strings.Contains(r.URL.Path, "/account_data/"):
		s.handleAccountData(w, r)
	case strings.Contains(r.URL.Path, "/send/m.room.message/"):
		s.handleSendMessage(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *matrixTestServer) handleAccountData(w http.ResponseWriter, r *http.Request) {
	eventType := path.Base(r.URL.Path)

	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		raw, ok := s.accountData[eventType]
		s.mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s.t.Fatalf("ReadAll(account data) error = %v", err)
		}
		s.mu.Lock()
		s.accountData[eventType] = append([]byte(nil), body...)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *matrixTestServer) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var content struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&content); err != nil {
		s.t.Fatalf("Decode(send body) error = %v", err)
	}

	s.mu.Lock()
	s.messages = append(s.messages, content.Body)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"event_id":"$reply:test"}`))
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
		URL:                baseURL + testAgentEndpointPath,
		ProtocolVersion:    string(a2aproto.Version),
		PreferredTransport: a2aproto.TransportProtocolJSONRPC,
		Capabilities:       a2aproto.AgentCapabilities{Streaming: false},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		AdditionalInterfaces: []a2aproto.AgentInterface{{
			Transport: a2aproto.TransportProtocolJSONRPC,
			URL:       baseURL + testAgentEndpointPath,
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

func currentUserText(msg *a2aproto.Message) string {
	if msg == nil {
		return ""
	}
	for _, part := range msg.Parts {
		switch value := part.(type) {
		case a2aproto.TextPart:
			return value.Text
		case *a2aproto.TextPart:
			return value.Text
		}
	}
	return ""
}
