package a2a

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"

	a2aproto "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
)

const mockA2AEndpointPath = "/rpc"

type RecordedNotification struct {
	Body           string
	Metadata       map[string]any
	MessageID      string
	TaskID         string
	ContextID      string
	ReferenceTasks []string
	History        []string
}

type MockServer struct {
	mu            sync.Mutex
	server        *httptest.Server
	notifications []RecordedNotification
	tasks         map[string]mockTask
	contextTasks  map[string]map[string]struct{}
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
	return &MockServer{
		tasks:        make(map[string]mockTask),
		contextTasks: make(map[string]map[string]struct{}),
	}
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

func (m *MockServer) Notifications() []RecordedNotification {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]RecordedNotification, len(m.notifications))
	copy(out, m.notifications)
	return out
}

func (m *MockServer) NotificationCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.notifications)
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
	}
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (m *MockServer) recordExecution(reqCtx *a2asrv.RequestContext) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	history, err := m.appendHistoryLocked(reqCtx)
	if err != nil {
		return err
	}

	referenceTasks := make([]string, 0, len(reqCtx.Message.ReferenceTasks))
	for _, taskID := range reqCtx.Message.ReferenceTasks {
		referenceTasks = append(referenceTasks, string(taskID))
	}
	m.notifications = append(m.notifications, RecordedNotification{
		Body:           currentUserText(reqCtx.Message),
		Metadata:       cloneMetadata(reqCtx.Metadata),
		MessageID:      reqCtx.Message.ID,
		TaskID:         string(reqCtx.TaskID),
		ContextID:      reqCtx.ContextID,
		ReferenceTasks: referenceTasks,
		History:        history,
	})
	return nil
}

type mockTask struct {
	TaskID     string
	ContextID  string
	History    []string
	ChildCount int
}

func (m *MockServer) appendHistoryLocked(reqCtx *a2asrv.RequestContext) ([]string, error) {
	body := currentUserText(reqCtx.Message)
	contextID := reqCtx.ContextID
	parent, err := m.resolveParentLocked(contextID, reqCtx.Message.ReferenceTasks)
	if err != nil {
		return nil, err
	}

	history := []string{body}
	if parent != nil {
		parent.ChildCount++
		m.tasks[parent.TaskID] = *parent
		history = append(append([]string(nil), parent.History...), body)
	}

	taskID := string(reqCtx.TaskID)
	m.tasks[taskID] = mockTask{
		TaskID:    taskID,
		ContextID: contextID,
		History:   history,
	}
	if m.contextTasks[contextID] == nil {
		m.contextTasks[contextID] = make(map[string]struct{})
	}
	m.contextTasks[contextID][taskID] = struct{}{}
	return append([]string(nil), history...), nil
}

func (m *MockServer) resolveParentLocked(contextID string, references []a2aproto.TaskID) (*mockTask, error) {
	switch {
	case len(references) > 1:
		return nil, fmt.Errorf("context %s has multiple references; supply exactly one referenceTaskId", contextID)
	case len(references) == 1:
		parent, ok := m.tasks[string(references[0])]
		if !ok || parent.ContextID != contextID {
			return nil, fmt.Errorf("reference task %s is not part of context %s", references[0], contextID)
		}
		return &parent, nil
	}

	var parent *mockTask
	for taskID := range m.contextTasks[contextID] {
		candidate := m.tasks[taskID]
		if candidate.ChildCount != 0 {
			continue
		}
		if parent != nil {
			return nil, fmt.Errorf("context %s has multiple task branches; specify referenceTaskIds", contextID)
		}
		parentCopy := candidate
		parent = &parentCopy
	}
	return parent, nil
}

func cloneMetadata(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}

	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

type mockExecutor struct {
	mock *MockServer
}

func (e *mockExecutor) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	if reqCtx.StoredTask == nil {
		if err := e.mock.recordExecution(reqCtx); err != nil {
			msg := a2aproto.NewMessageForTask(
				a2aproto.MessageRoleAgent,
				reqCtx,
				a2aproto.TextPart{Text: err.Error()},
			)
			update := a2aproto.NewStatusUpdateEvent(reqCtx, a2aproto.TaskStateFailed, msg)
			update.Final = true
			if writeErr := queue.Write(ctx, update); writeErr != nil {
				return fmt.Errorf("write failed status update: %w", writeErr)
			}
			return nil
		}
	}

	msg := a2aproto.NewMessageForTask(
		a2aproto.MessageRoleAgent,
		reqCtx,
		a2aproto.TextPart{Text: "received"},
	)
	update := a2aproto.NewStatusUpdateEvent(reqCtx, a2aproto.TaskStateCompleted, msg)
	update.Final = true

	if err := queue.Write(ctx, update); err != nil {
		return fmt.Errorf("write status update: %w", err)
	}
	return nil
}

func (e *mockExecutor) Cancel(context.Context, *a2asrv.RequestContext, eventqueue.Queue) error {
	return nil
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
