package a2a

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	a2aproto "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
)

func TestResponseReady(t *testing.T) {
	tests := []struct {
		name string
		resp Response
		want bool
	}{
		{
			name: "submitted without reply keeps polling",
			resp: Response{State: a2aproto.TaskStateSubmitted},
			want: false,
		},
		{
			name: "working without reply keeps polling",
			resp: Response{State: a2aproto.TaskStateWorking},
			want: false,
		},
		{
			name: "working with reply returns",
			resp: Response{State: a2aproto.TaskStateWorking, Reply: "partial"},
			want: true,
		},
		{
			name: "input required without reply returns",
			resp: Response{State: a2aproto.TaskStateInputRequired},
			want: true,
		},
		{
			name: "terminal without reply returns",
			resp: Response{State: a2aproto.TaskStateCompleted},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := responseReady(tt.resp); got != tt.want {
				t.Fatalf("responseReady(%+v) = %t, want %t", tt.resp, got, tt.want)
			}
		})
	}
}

func TestClientSendWaitsForUsableReplyAfterWorkingTask(t *testing.T) {
	const (
		taskID    = "task-123"
		contextID = "ctx-123"
		replyText = "ready"
	)

	var (
		mu           sync.Mutex
		getTaskCalls int
	)

	server := newLocalServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case a2asrv.WellKnownAgentCardPath:
			a2asrv.NewStaticAgentCardHandler(testAgentCard(serverBaseURL(r))).ServeHTTP(w, r)
			return
		case mockA2AEndpointPath:
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
			if params.Config == nil || params.Config.Blocking == nil || !*params.Config.Blocking {
				t.Fatalf("message/send blocking = %#v, want true", params.Config)
			}

			writeJSONRPCResponse(t, w, req.ID, map[string]any{
				"kind":      "task",
				"id":        taskID,
				"contextId": contextID,
				"status": map[string]any{
					"state": "working",
				},
			})
		case "tasks/get":
			mu.Lock()
			getTaskCalls++
			callNumber := getTaskCalls
			mu.Unlock()

			result := map[string]any{
				"kind":      "task",
				"id":        taskID,
				"contextId": contextID,
				"status": map[string]any{
					"state": "working",
				},
			}
			if callNumber >= 3 {
				result["status"] = map[string]any{
					"state": "completed",
					"message": map[string]any{
						"kind":      "message",
						"messageId": "msg-123",
						"role":      "agent",
						"parts": []map[string]any{{
							"kind": "text",
							"text": replyText,
						}},
					},
				}
			}

			writeJSONRPCResponse(t, w, req.ID, result)
		default:
			t.Fatalf("unexpected JSON-RPC method %q", req.Method)
		}
	}))

	client, err := New(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	resp, err := client.Send(context.Background(), Request{Text: "hello"})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if resp.TaskID != taskID || resp.ContextID != contextID {
		t.Fatalf("response identity = (%q, %q), want (%q, %q)", resp.TaskID, resp.ContextID, taskID, contextID)
	}
	if resp.State != a2aproto.TaskStateCompleted {
		t.Fatalf("resp.State = %s, want %s", resp.State, a2aproto.TaskStateCompleted)
	}
	if resp.Reply != replyText {
		t.Fatalf("resp.Reply = %q, want %q", resp.Reply, replyText)
	}

	mu.Lock()
	calls := getTaskCalls
	mu.Unlock()
	if calls < 3 {
		t.Fatalf("GetTask was called %d times, want at least 3 polls before completion", calls)
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
		URL:                baseURL + mockA2AEndpointPath,
		ProtocolVersion:    string(a2aproto.Version),
		PreferredTransport: a2aproto.TransportProtocolJSONRPC,
		Capabilities:       a2aproto.AgentCapabilities{Streaming: false},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		AdditionalInterfaces: []a2aproto.AgentInterface{{
			Transport: a2aproto.TransportProtocolJSONRPC,
			URL:       baseURL + mockA2AEndpointPath,
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
