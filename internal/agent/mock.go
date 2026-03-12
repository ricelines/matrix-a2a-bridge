package agent

import (
	"context"
	"encoding/gob"
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

const mockAgentEndpointPath = "/rpc"

func init() {
	gob.Register(map[string]any{})
	gob.Register([]map[string]any{})
}

type MockServer struct {
	mu           sync.Mutex
	server       *httptest.Server
	roomTaskIDs  map[string][]string
	userContexts map[string]map[string]struct{}
	canceled     []string
}

type responsePlan struct {
	State              a2aproto.TaskState
	Reply              string
	Commands           []Command
	CompleteOnboarding bool
	CloseSession       bool
}

func StartMockServer() *MockServer {
	mock := &MockServer{
		roomTaskIDs:  make(map[string][]string),
		userContexts: make(map[string]map[string]struct{}),
	}

	reqHandler := a2asrv.NewHandler(&mockExecutor{mock: mock})
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	mock.server = server

	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(mock.agentCard()))
	mux.Handle(mockAgentEndpointPath, a2asrv.NewJSONRPCHandler(reqHandler))

	return mock
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

func (m *MockServer) agentCard() *a2aproto.AgentCard {
	return &a2aproto.AgentCard{
		Name:               "Mock Ricelines A2A Agent",
		Description:        "Mock onboarding and intent parser used by the Matrix onboarding bot tests.",
		Version:            "test",
		URL:                m.server.URL + mockAgentEndpointPath,
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
				URL:       m.server.URL + mockAgentEndpointPath,
			},
		},
		Skills: []a2aproto.AgentSkill{},
	}
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

	msg := buildAgentMessage(
		reqCtx,
		"I'll close this session here. Message me again when you're ready to continue.",
		controlPayload{CloseSession: true},
	)
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
	command, hasCommand := parseCommand(text)

	if mode == "onboarding" {
		return onboardingPlan(reqCtx, command, hasCommand)
	}
	return generalPlan(command, hasCommand)
}

func onboardingPlan(reqCtx *a2asrv.RequestContext, command Command, hasCommand bool) responsePlan {
	_, trigger := workflowMetadata(reqCtx.Metadata)
	if hasCommand && command.Name == "close_session" {
		return responsePlan{
			State:        a2aproto.TaskStateCompleted,
			Reply:        "No problem. We can pick up onboarding again the next time you message me.",
			CloseSession: true,
		}
	}
	if hasCommand {
		return responsePlan{
			State:    a2aproto.TaskStateInputRequired,
			Reply:    "I can answer that while we finish onboarding. Once we're done, I'll stay available for follow-up requests.",
			Commands: []Command{command},
		}
	}

	if reqCtx.StoredTask == nil {
		if trigger == "invite" {
			return responsePlan{
				State: a2aproto.TaskStateInputRequired,
				Reply: "Welcome to Ricelines. I'll get you oriented. What should I call you?",
			}
		}
		return responsePlan{
			State: a2aproto.TaskStateInputRequired,
			Reply: "Welcome to Ricelines. Before we get started, what should I call you?",
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
			State:              a2aproto.TaskStateCompleted,
			Reply:              "Thanks. You're onboarded now, and you can keep chatting with me naturally whenever you need help.",
			CompleteOnboarding: true,
		}
	}
}

func generalPlan(command Command, hasCommand bool) responsePlan {
	if hasCommand && command.Name == "close_session" {
		return responsePlan{
			State:        a2aproto.TaskStateCompleted,
			Reply:        "I'll wrap up this session now.",
			Commands:     []Command{command},
			CloseSession: true,
		}
	}
	if hasCommand {
		reply := "I can help with that."
		if command.Name == "status" {
			reply = "I can check that."
		}
		return responsePlan{
			State:    a2aproto.TaskStateInputRequired,
			Reply:    reply,
			Commands: []Command{command},
		}
	}

	return responsePlan{
		State: a2aproto.TaskStateInputRequired,
		Reply: "I'm here, and the command-routing layer is wired up. The concrete actions are still stubs, so ask for help or status if you want to see what's connected.",
	}
}

func buildStatusMessage(reqCtx *a2asrv.RequestContext, plan responsePlan) *a2aproto.Message {
	return buildAgentMessage(reqCtx, plan.Reply, controlPayload{
		Commands:           plan.Commands,
		CompleteOnboarding: plan.CompleteOnboarding,
		CloseSession:       plan.CloseSession,
	})
}

func buildAgentMessage(reqCtx *a2asrv.RequestContext, reply string, payload controlPayload) *a2aproto.Message {
	parts := make([]a2aproto.Part, 0, 2)
	if len(payload.Commands) > 0 || payload.CompleteOnboarding || payload.CloseSession {
		commands := make([]map[string]any, 0, len(payload.Commands))
		for _, command := range payload.Commands {
			commands = append(commands, map[string]any{
				"name": command.Name,
				"args": command.Args,
			})
		}
		data := map[string]any{
			"commands":            commands,
			"complete_onboarding": payload.CompleteOnboarding,
			"close_session":       payload.CloseSession,
		}
		parts = append(parts, a2aproto.DataPart{Data: data})
	}
	if reply != "" {
		parts = append(parts, a2aproto.TextPart{Text: reply})
	}
	return a2aproto.NewMessageForTask(a2aproto.MessageRoleAgent, reqCtx, parts...)
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

func parseCommand(text string) (Command, bool) {
	normalized := strings.ToLower(strings.TrimSpace(text))
	switch {
	case normalized == "":
		return Command{}, false
	case strings.Contains(normalized, "status"):
		return Command{Name: "status"}, true
	case strings.Contains(normalized, "help"), strings.Contains(normalized, "what can you do"):
		return Command{Name: "help"}, true
	case strings.Contains(normalized, "close"), strings.Contains(normalized, "done for now"):
		return Command{Name: "close_session"}, true
	default:
		return Command{}, false
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
