package a2a

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	a2aproto "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
)

func TestClientDeliverSendsNonBlockingNotification(t *testing.T) {
	const body = `{"kind":"matrix_event"}`

	var gotMetadata map[string]any
	var gotContextID string
	var gotReferenceTaskIDs []a2aproto.TaskID
	var gotMessageID string

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
			if params.Config == nil || params.Config.Blocking == nil || *params.Config.Blocking {
				t.Fatalf("message/send blocking = %#v, want false", params.Config)
			}

			if got := currentUserText(params.Message); got != body {
				t.Fatalf("message/send body = %q, want %q", got, body)
			}
			gotMetadata = params.Metadata
			gotContextID = params.Message.ContextID
			gotReferenceTaskIDs = append([]a2aproto.TaskID(nil), params.Message.ReferenceTasks...)
			gotMessageID = params.Message.ID

			writeJSONRPCResponse(t, w, req.ID, map[string]any{
				"kind":      "task",
				"id":        "task-123",
				"contextId": "ctx-123",
				"status": map[string]any{
					"state": "submitted",
				},
			})
		default:
			t.Fatalf("unexpected JSON-RPC method %q", req.Method)
		}
	}))

	client, err := New(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	task, err := client.Deliver(context.Background(), Notification{
		Body: body,
		Metadata: map[string]any{
			"matrix_event": map[string]any{
				"kind": "matrix_event",
			},
		},
	}, DeliveryOptions{
		MessageID:       "msg-123",
		ContextID:       "ctx-123",
		ReferenceTaskID: "task-122",
	})
	if err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	if task.ID != "task-123" || task.ContextID != "ctx-123" {
		t.Fatalf("Deliver() task = %#v, want returned task identity", task)
	}

	matrixEvent, ok := gotMetadata["matrix_event"].(map[string]any)
	if !ok || matrixEvent["kind"] != "matrix_event" {
		t.Fatalf("metadata = %#v, want matrix_event.kind", gotMetadata)
	}
	if gotContextID != "ctx-123" {
		t.Fatalf("message/send contextId = %q, want %q", gotContextID, "ctx-123")
	}
	if len(gotReferenceTaskIDs) != 1 || gotReferenceTaskIDs[0] != "task-122" {
		t.Fatalf("message/send referenceTaskIds = %#v, want [task-122]", gotReferenceTaskIDs)
	}
	if gotMessageID != "msg-123" {
		t.Fatalf("message/send messageId = %q, want %q", gotMessageID, "msg-123")
	}
}

func TestClientDeliverRejectsEmptyBody(t *testing.T) {
	client := &Client{}

	_, err := client.Deliver(context.Background(), Notification{Body: " \n\t "}, DeliveryOptions{})
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("Deliver() error = %v, want empty body validation", err)
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
