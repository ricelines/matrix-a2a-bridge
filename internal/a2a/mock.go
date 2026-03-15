package a2a

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"

	a2aproto "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
)

const mockA2AEndpointPath = "/rpc"

type MockServer struct {
	mu           sync.Mutex
	server       *httptest.Server
	roomTaskIDs  map[string][]string
	userContexts map[string]map[string]struct{}
	canceled     []string
}

type responsePlan struct {
	State a2aproto.TaskState
	Reply string
}

func StartMockServer() *MockServer {
	mock := newMockServer()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("listen mock A2A server: %v", err))
	}
	server := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: newMockHandler(mock)},
	}
	server.Start()
	mock.server = server

	return mock
}

func NewMockHTTPHandler() http.Handler {
	return newMockHandler(newMockServer())
}

func newMockServer() *MockServer {
	mock := &MockServer{
		roomTaskIDs:  make(map[string][]string),
		userContexts: make(map[string]map[string]struct{}),
	}
	return mock
}

func newMockHandler(mock *MockServer) http.Handler {
	reqHandler := a2asrv.NewHandler(&mockExecutor{mock: mock})
	mux := http.NewServeMux()
	mux.HandleFunc(a2asrv.WellKnownAgentCardPath, func(w http.ResponseWriter, r *http.Request) {
		a2asrv.NewStaticAgentCardHandler(mock.agentCard(requestBaseURL(r))).ServeHTTP(w, r)
	})
	mux.Handle(mockA2AEndpointPath, a2asrv.NewJSONRPCHandler(reqHandler))
	return mux
}

func (m *MockServer) Close() {
	if m.server != nil {
		m.server.Close()
	}
}

func (m *MockServer) BaseURL() string {
	if m.server == nil {
		return ""
	}
	return m.server.URL
}

func (m *MockServer) TaskCountForRoom(roomID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.roomTaskIDs[roomID])
}

func (m *MockServer) TaskIDsForRoom(roomID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.roomTaskIDs[roomID]...)
}

func (m *MockServer) ContextCountForUser(userID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.userContexts[userID])
}

func (m *MockServer) WasTaskCanceled(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return slices.Contains(m.canceled, taskID)
}

func (m *MockServer) agentCard(baseURL string) *a2aproto.AgentCard {
	return &a2aproto.AgentCard{
		Name:               "Mock Upstream A2A Agent",
		Description:        "Mock upstream A2A endpoint used by the Matrix A2A bridge tests.",
		Version:            "test",
		URL:                baseURL + mockA2AEndpointPath,
		ProtocolVersion:    string(a2aproto.Version),
		PreferredTransport: a2aproto.TransportProtocolJSONRPC,
		Capabilities: a2aproto.AgentCapabilities{
			Streaming: false,
		},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain", "application/json"},
		AdditionalInterfaces: []a2aproto.AgentInterface{
			{
				Transport: a2aproto.TransportProtocolJSONRPC,
				URL:       baseURL + mockA2AEndpointPath,
			},
		},
		Skills: []a2aproto.AgentSkill{},
	}
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (m *MockServer) recordExecution(reqCtx *a2asrv.RequestContext) {
	roomID, userID := matrixMetadata(reqCtx.Metadata)
	if roomID == "" || userID == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.roomTaskIDs[roomID] = append(m.roomTaskIDs[roomID], string(reqCtx.TaskID))
	if _, ok := m.userContexts[userID]; !ok {
		m.userContexts[userID] = make(map[string]struct{})
	}
	m.userContexts[userID][reqCtx.ContextID] = struct{}{}
}

func (m *MockServer) recordCancel(taskID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.canceled = append(m.canceled, taskID)
}

type mockExecutor struct {
	mock *MockServer
}

func (e *mockExecutor) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	if reqCtx.StoredTask == nil {
		e.mock.recordExecution(reqCtx)
		if err := queue.Write(ctx, a2aproto.NewSubmittedTask(reqCtx, reqCtx.Message)); err != nil {
			return fmt.Errorf("write submitted task: %w", err)
		}
	}

	plan := buildPlan(reqCtx)
	statusMessage := buildStatusMessage(reqCtx, plan)
	update := a2aproto.NewStatusUpdateEvent(reqCtx, plan.State, statusMessage)
	update.Final = plan.State.Terminal() || plan.State == a2aproto.TaskStateInputRequired

	if err := queue.Write(ctx, update); err != nil {
		return fmt.Errorf("write status update: %w", err)
	}
	return nil
}

func (e *mockExecutor) Cancel(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	e.mock.recordCancel(string(reqCtx.TaskID))

	msg := buildAgentMessage(reqCtx, "I'll close this task here. Message me again when you're ready to continue.")
	update := a2aproto.NewStatusUpdateEvent(reqCtx, a2aproto.TaskStateCanceled, msg)
	update.Final = true

	if err := queue.Write(ctx, update); err != nil {
		return fmt.Errorf("write cancel status: %w", err)
	}
	return nil
}

func buildPlan(reqCtx *a2asrv.RequestContext) responsePlan {
	text := strings.TrimSpace(currentUserText(reqCtx.Message))
	intent, hasIntent := parseIntent(text)

	if hasIntent && intent == "close_session" {
		return responsePlan{
			State: a2aproto.TaskStateCompleted,
			Reply: "I'll wrap up this A2A task now.",
		}
	}
	if hasIntent {
		reply := "I can keep this DM connected to an A2A task, preserve shared context per Matrix user, and continue the conversation naturally."
		if intent == "status" {
			reply = "This Matrix DM is connected to an A2A task and reuses the shared context for this Matrix user."
		}
		return responsePlan{
			State: a2aproto.TaskStateInputRequired,
			Reply: reply,
		}
	}

	if reqCtx.StoredTask != nil {
		return responsePlan{
			State: a2aproto.TaskStateInputRequired,
			Reply: "I'm still connected to the same A2A task. Ask for help, ask for status, or tell me to close the task.",
		}
	}

	return responsePlan{
		State: a2aproto.TaskStateInputRequired,
		Reply: "I received your message over Matrix and opened an A2A task. Ask for help, ask for status, or tell me to close the task.",
	}
}

func buildStatusMessage(reqCtx *a2asrv.RequestContext, plan responsePlan) *a2aproto.Message {
	return buildAgentMessage(reqCtx, plan.Reply)
}

func buildAgentMessage(reqCtx *a2asrv.RequestContext, reply string) *a2aproto.Message {
	return a2aproto.NewMessageForTask(
		a2aproto.MessageRoleAgent,
		reqCtx,
		a2aproto.TextPart{Text: reply},
	)
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

func parseIntent(text string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(text))
	switch {
	case normalized == "":
		return "", false
	case strings.Contains(normalized, "status"):
		return "status", true
	case strings.Contains(normalized, "help"), strings.Contains(normalized, "what can you do"):
		return "help", true
	case strings.Contains(normalized, "close"), strings.Contains(normalized, "done for now"):
		return "close_session", true
	default:
		return "", false
	}
}

func matrixMetadata(metadata map[string]any) (roomID string, userID string) {
	matrix, ok := metadata["matrix"].(map[string]any)
	if !ok {
		return "", ""
	}
	roomID, _ = matrix["room_id"].(string)
	userID, _ = matrix["user_id"].(string)
	return roomID, userID
}
