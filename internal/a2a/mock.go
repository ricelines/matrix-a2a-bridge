package a2a

import (
	"context"
	"fmt"
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
	server := httptest.NewServer(newMockHandler(mock))
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
		Description:        "Mock upstream A2A endpoint used by onboarding-agent's Matrix runtime tests.",
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

	msg := buildAgentMessage(reqCtx, "I'll close this session here. Message me again when you're ready to continue.")
	update := a2aproto.NewStatusUpdateEvent(reqCtx, a2aproto.TaskStateCanceled, msg)
	update.Final = true

	if err := queue.Write(ctx, update); err != nil {
		return fmt.Errorf("write cancel status: %w", err)
	}
	return nil
}

func buildPlan(reqCtx *a2asrv.RequestContext) responsePlan {
	mode, _ := workflowMetadata(reqCtx.Metadata)
	text := strings.TrimSpace(currentUserText(reqCtx.Message))
	intent, hasIntent := parseIntent(text)

	if mode == "onboarding" {
		return onboardingPlan(reqCtx, intent, hasIntent)
	}
	return generalPlan(intent, hasIntent)
}

func onboardingPlan(reqCtx *a2asrv.RequestContext, intent string, hasIntent bool) responsePlan {
	if reqCtx.StoredTask == nil {
		return responsePlan{
			State: a2aproto.TaskStateInputRequired,
			Reply: "Welcome to Ricelines. I'll get you oriented. What should I call you?",
		}
	}

	if hasIntent && intent == "close_session" {
		return responsePlan{
			State: a2aproto.TaskStateCompleted,
			Reply: "No problem. We can pick up onboarding again the next time you message me.",
		}
	}
	if hasIntent {
		reply := "We’re still onboarding. Answer the next question and I’ll stay available for follow-up requests after that."
		if intent == "status" {
			reply = "You're still in onboarding. Once we finish, I'll keep helping in this DM."
		}
		return responsePlan{
			State: a2aproto.TaskStateInputRequired,
			Reply: reply,
		}
	}

	switch len(reqCtx.StoredTask.History) {
	case 2:
		return responsePlan{
			State: a2aproto.TaskStateInputRequired,
			Reply: "What brings you to Ricelines today?",
		}
	default:
		return responsePlan{
			State: a2aproto.TaskStateCompleted,
			Reply: "Thanks. You're onboarded now, and you can keep chatting with me naturally whenever you need help.",
		}
	}
}

func generalPlan(intent string, hasIntent bool) responsePlan {
	if hasIntent && intent == "close_session" {
		return responsePlan{
			State: a2aproto.TaskStateCompleted,
			Reply: "I'll wrap up this session now.",
		}
	}
	if hasIntent {
		reply := "I can keep this DM connected to an A2A task, preserve shared context, and continue the conversation naturally."
		if intent == "status" {
			reply = "Your onboarding record is set, and this conversation is using the shared A2A context."
		}
		return responsePlan{
			State: a2aproto.TaskStateInputRequired,
			Reply: reply,
		}
	}

	return responsePlan{
		State: a2aproto.TaskStateInputRequired,
		Reply: "I'm here and ready to continue the conversation over A2A. Ask for help or status if you want to inspect the mock flow.",
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

func workflowMetadata(metadata map[string]any) (mode string, trigger string) {
	workflow, ok := metadata["workflow"].(map[string]any)
	if !ok {
		return "", ""
	}
	mode, _ = workflow["mode"].(string)
	trigger, _ = workflow["trigger"].(string)
	return mode, trigger
}
