package bridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"matrix-a2a-bridge/internal/state"
)

const (
	codexPromptTimeout      = 30 * time.Second
	codexServerReadyTimeout = 45 * time.Second
)

func TestBridgeAppendsPromptHistoryPerRoomWithRealCodexA2A(t *testing.T) {
	suite := startRealCodexPromptSuite(t)

	roomA := suite.createPrivateRoomWithBotInvite(t)
	suite.joinRoomAsBot(t, roomA)
	waitForJoinedMember(t, suite.alice.client, roomA, suite.botUserID, joinTimeout)

	roomB := suite.createPrivateRoomWithBotInvite(t)
	suite.joinRoomAsBot(t, roomB)
	waitForJoinedMember(t, suite.alice.client, roomB, suite.botUserID, joinTimeout)

	baseline := suite.recorder.RequestCount()

	suite.alice.sendText(t, roomA, "Hi. My name is Alice")
	firstA := suite.waitForResponsesRequestSince(t, baseline, codexPromptTimeout, func(req capturedResponseRequest) bool {
		return requestContainsUserText(t, req, "Hi. My name is Alice")
	}, "first room A message did not reach codex responses")
	firstATexts := firstA.userInputTexts(t)
	assertContainsText(t, firstATexts, "Hi. My name is Alice")
	assertDoesNotContainText(t, firstATexts, "Hi. what is my name?")

	baseline = suite.recorder.RequestCount()
	suite.alice.sendText(t, roomA, "What is my name?")
	secondA := suite.waitForResponsesRequestSince(t, baseline, codexPromptTimeout, func(req capturedResponseRequest) bool {
		return requestContainsUserText(t, req, "What is my name?")
	}, "second room A message did not reach codex responses")
	secondATexts := secondA.userInputTexts(t)
	assertContainsText(t, secondATexts, "Hi. My name is Alice")
	assertContainsText(t, secondATexts, "What is my name?")
	assertDoesNotContainText(t, secondATexts, "Hi. what is my name?")

	baseline = suite.recorder.RequestCount()
	suite.alice.sendText(t, roomB, "Hi. what is my name?")
	firstB := suite.waitForResponsesRequestSince(t, baseline, codexPromptTimeout, func(req capturedResponseRequest) bool {
		return requestContainsUserText(t, req, "Hi. what is my name?")
	}, "room B message did not reach codex responses")
	firstBTexts := firstB.userInputTexts(t)
	assertContainsText(t, firstBTexts, "Hi. what is my name?")
	assertDoesNotContainText(t, firstBTexts, "Hi. My name is Alice")
	assertDoesNotContainText(t, firstBTexts, "What is my name?")
}

type realCodexPromptSuite struct {
	server          *tuwunelContainer
	recorder        *responsesRecorder
	responsesServer *loopbackServer
	alice           *syncingClient
	botControl      *mautrix.Client
	codexRun        *runningHTTPProcess
	bridgeRun       *runningBridge
	bridgeLog       *bytes.Buffer
	statePath       string
	botUserID       id.UserID
}

func startRealCodexPromptSuite(t *testing.T) *realCodexPromptSuite {
	t.Helper()
	requireLiveBridgeTests(t)
	if testing.Short() {
		t.Fatal("real codex live tests were explicitly requested in short mode")
	}
	if err := ensureDocker(); err != nil {
		t.Fatalf("ensureDocker() error = %v", err)
	}

	var suite *realCodexPromptSuite
	var cleanups []func()
	defer func() {
		if suite != nil {
			return
		}
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}()

	server, err := startTuwunelContainer()
	if err != nil {
		t.Fatalf("startTuwunelContainer() error = %v", err)
	}
	cleanups = append(cleanups, func() { _ = server.Close() })

	recorder := newResponsesRecorder("bridge prompt capture")
	responsesServer := recorder.server(t)
	cleanups = append(cleanups, responsesServer.Close)

	alice, err := startSyncingClient(server.baseURL(), aliceUser, alicePassword)
	if err != nil {
		t.Fatalf("startSyncingClient() error = %v", err)
	}
	cleanups = append(cleanups, func() { _ = alice.Close() })

	botControl, err := newLoggedInClient(server.baseURL(), botUser, botPassword)
	if err != nil {
		t.Fatalf("newLoggedInClient() error = %v", err)
	}

	codexWorkspace := t.TempDir()
	codexRun := startCodexA2AProcess(t, codexWorkspace, responsesServer.URL)
	cleanups = append(cleanups, func() { _ = codexRun.Close() })

	bridgeLog := &bytes.Buffer{}
	statePath := filepath.Join(server.tempDir, "bridge-state.json")
	bridgeRun, err := startBridgeRuntime(server.baseURL(), codexRun.baseURL, statePath, bridgeLog)
	if err != nil {
		t.Fatalf("startBridgeRuntime() error = %v", err)
	}
	cleanups = append(cleanups, func() { _ = bridgeRun.Close() })

	suite = &realCodexPromptSuite{
		server:          server,
		recorder:        recorder,
		responsesServer: responsesServer,
		alice:           alice,
		botControl:      botControl,
		codexRun:        codexRun,
		bridgeRun:       bridgeRun,
		bridgeLog:       bridgeLog,
		statePath:       statePath,
		botUserID:       id.UserID(fmt.Sprintf("@%s:%s", botUser, serverName)),
	}

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("bridge logs:\n%s", suite.bridgeLog.String())
			t.Logf("codex-a2a logs:\n%s", suite.codexRun.logs.String())
			t.Logf("mock responses requests:\n%s", suite.recorder.dumpRequests())
			t.Logf("tuwunel logs:\n%s", suite.server.logs())
		}
		if err := suite.Close(); err != nil {
			t.Fatalf("suite.Close() error = %v", err)
		}
	})

	if err := waitUntilBridgeReady(statePath, suite.botUserID); err != nil {
		t.Fatalf("waitUntilBridgeReady() error = %v", err)
	}

	return suite
}

func (s *realCodexPromptSuite) Close() error {
	var errs []error

	if s.bridgeRun != nil {
		if err := s.bridgeRun.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.codexRun != nil {
		if err := s.codexRun.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.alice != nil {
		if err := s.alice.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.responsesServer != nil {
		s.responsesServer.Close()
	}
	if s.server != nil {
		if err := s.server.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (s *realCodexPromptSuite) createPrivateRoomWithBotInvite(t *testing.T) id.RoomID {
	t.Helper()

	room, err := s.alice.client.CreateRoom(context.Background(), &mautrix.ReqCreateRoom{
		Preset:   "private_chat",
		IsDirect: false,
	})
	if err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}

	if _, err := s.alice.client.InviteUser(context.Background(), room.RoomID, &mautrix.ReqInviteUser{
		UserID: s.botUserID,
	}); err != nil {
		t.Fatalf("InviteUser() error = %v", err)
	}

	return room.RoomID
}

func (s *realCodexPromptSuite) joinRoomAsBot(t *testing.T, roomID id.RoomID) {
	t.Helper()
	if _, err := s.botControl.JoinRoomByID(context.Background(), roomID); err != nil {
		t.Fatalf("JoinRoomByID() error = %v", err)
	}
}

func (s *realCodexPromptSuite) waitForResponsesRequestSince(
	t *testing.T,
	start int,
	timeout time.Duration,
	predicate func(capturedResponseRequest) bool,
	message string,
) capturedResponseRequest {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		reqs := s.recorder.requests()
		for i := start; i < len(reqs); i++ {
			if predicate(reqs[i]) {
				return reqs[i]
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("%s\ncaptured requests:\n%s", message, s.recorder.dumpRequests())
	return capturedResponseRequest{}
}

type runningHTTPProcess struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	done    chan struct{}
	baseURL string
	logs    *bytes.Buffer

	mu      sync.Mutex
	waitErr error
}

func startCodexA2AProcess(t *testing.T, workspace, responsesServerURL string) *runningHTTPProcess {
	t.Helper()

	port, err := reserveHostPort()
	if err != nil {
		t.Fatalf("reserveHostPort() error = %v", err)
	}

	codexRepo := siblingRepoPath(t, "codex-a2a")
	codexHome := t.TempDir()
	if err := writeMockCodexResponsesConfig(filepath.Join(codexHome, "config.toml"), responsesServerURL); err != nil {
		t.Fatalf("writeMockCodexResponsesConfig() error = %v", err)
	}

	codexBin := findRealCodexBinary(t)
	serverBin := buildCodexA2ABinary(t, codexRepo)
	baseURL := "http://127.0.0.1:" + port
	logBuf := &bytes.Buffer{}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, serverBin,
		"--listen", "127.0.0.1:"+port,
		"--default-cwd", workspace,
		"--default-model", "mock-model",
		"--default-approval-policy", "never",
		"--default-sandbox", "read-only",
		"--codex-cli", codexBin,
	)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start codex-a2a error = %v", err)
	}

	proc := &runningHTTPProcess{
		cmd:     cmd,
		cancel:  cancel,
		done:    make(chan struct{}),
		baseURL: baseURL,
		logs:    logBuf,
	}
	go func() {
		err := cmd.Wait()
		proc.mu.Lock()
		proc.waitErr = err
		proc.mu.Unlock()
		close(proc.done)
	}()

	if err := waitForHTTPReady(baseURL+"/.well-known/agent-card.json", codexServerReadyTimeout, proc); err != nil {
		_ = proc.Close()
		t.Fatalf("waitForHTTPReady(codex-a2a) error = %v", err)
	}

	return proc
}

func (p *runningHTTPProcess) Close() error {
	if p == nil {
		return nil
	}

	p.cancel()
	select {
	case <-p.done:
		return p.closeResult()
	case <-time.After(2 * time.Second):
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	}

	select {
	case <-p.done:
		return p.closeResult()
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timed out waiting for process to stop")
	}
}

func (p *runningHTTPProcess) closeResult() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waitErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(p.waitErr, &exitErr) {
		return nil
	}
	return fmt.Errorf("process stopped with error: %w", p.waitErr)
}

func waitForHTTPReady(url string, timeout time.Duration, proc *runningHTTPProcess) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}

		select {
		case <-proc.done:
			proc.mu.Lock()
			err := proc.waitErr
			proc.mu.Unlock()
			return fmt.Errorf("process exited before ready: %v\n%s", err, proc.logs.String())
		default:
		}

		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s\n%s", url, proc.logs.String())
}

func waitUntilBridgeReady(statePath string, botUserID id.UserID) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		store, err := state.Open(statePath)
		if err == nil {
			snapshot := store.Snapshot()
			if snapshot.Session.UserID == botUserID.String() && snapshot.Sync.NextBatch != "" {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("bridge did not finish initial sync")
}

func findRealCodexBinary(t *testing.T) string {
	t.Helper()

	if path := os.Getenv("CODEX_A2A_REAL_CODEX_BIN"); path != "" {
		return path
	}
	path, err := exec.LookPath("codex")
	if err == nil {
		return path
	}
	t.Skip("real codex binary not found; set CODEX_A2A_REAL_CODEX_BIN or install codex")
	return ""
}

func buildCodexA2ABinary(t *testing.T, repoDir string) string {
	t.Helper()

	binPath := filepath.Join(t.TempDir(), "codex-a2a-test-bin")
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/codex-a2a")
	cmd.Dir = repoDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build codex-a2a error = %v\n%s", err, string(output))
	}
	return binPath
}

func siblingRepoPath(t *testing.T, repoName string) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	for {
		if filepath.Base(dir) == "matrix-a2a-bridge" {
			path := filepath.Join(filepath.Dir(dir), repoName)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("Stat(%s) error = %v", path, err)
			}
			if !info.IsDir() {
				t.Fatalf("%s is not a directory", path)
			}
			return path
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate matrix-a2a-bridge repo root from %s", dir)
		}
		dir = parent
	}
}

func writeMockCodexResponsesConfig(path, serverURL string) error {
	config := fmt.Sprintf(`
model = "mock-model"
model_provider = "mock_provider"
approval_policy = "never"
sandbox_mode = "read-only"
enable_request_compression = false

[features]

[model_providers.mock_provider]
name = "Mock provider for bridge prompt test"
base_url = "%s/v1"
wire_api = "responses"
request_max_retries = 0
stream_max_retries = 0
requires_openai_auth = false
supports_websockets = false
`, serverURL)
	return os.WriteFile(path, []byte(strings.TrimSpace(config)+"\n"), 0o644)
}

type responsesRecorder struct {
	mu       sync.Mutex
	response string
	reqs     []capturedResponseRequest
}

type capturedResponseRequest struct {
	Body []byte
}

func newResponsesRecorder(response string) *responsesRecorder {
	return &responsesRecorder{response: response}
}

type loopbackServer struct {
	URL      string
	server   *http.Server
	listener net.Listener
}

func (s *loopbackServer) Close() {
	_ = s.server.Close()
	_ = s.listener.Close()
}

func (r *responsesRecorder) server(t *testing.T) *loopbackServer {
	t.Helper()

	handler := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/v1/responses" {
			http.NotFound(rw, req)
			return
		}

		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		r.mu.Lock()
		r.reqs = append(r.reqs, capturedResponseRequest{
			Body: body,
		})
		r.mu.Unlock()

		rw.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(rw, fmt.Sprintf(
			"event: response.created\n"+
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n"+
				"event: response.output_item.done\n"+
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}}\n\n"+
				"event: response.completed\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[]}}\n\n",
			r.response,
		))
	})

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("sandbox cannot open a loopback listener for mock responses server: %v", err)
	}
	server := &http.Server{Handler: handler}
	go func() {
		err := server.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}()
	return &loopbackServer{
		URL:      "http://" + listener.Addr().String(),
		server:   server,
		listener: listener,
	}
}

func (r *responsesRecorder) RequestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func (r *responsesRecorder) requests() []capturedResponseRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]capturedResponseRequest, len(r.reqs))
	copy(out, r.reqs)
	return out
}

func (r *responsesRecorder) dumpRequests() string {
	reqs := r.requests()
	if len(reqs) == 0 {
		return "<none>"
	}

	var out strings.Builder
	for idx, req := range reqs {
		if idx > 0 {
			out.WriteString("\n\n")
		}
		fmt.Fprintf(&out, "request %d:\n%s", idx+1, string(req.Body))
	}
	return out.String()
}

func (r capturedResponseRequest) userInputTexts(t *testing.T) []string {
	t.Helper()

	var body struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("Unmarshal(request body) error = %v\nbody=%s", err, string(r.Body))
	}

	var texts []string
	for _, item := range body.Input {
		if item["type"] != "message" || item["role"] != "user" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, raw := range content {
			entry, _ := raw.(map[string]any)
			if entry["type"] == "input_text" {
				if text, _ := entry["text"].(string); text != "" {
					texts = append(texts, text)
				}
			}
		}
	}
	return texts
}

func requestContainsUserText(t *testing.T, req capturedResponseRequest, want string) bool {
	t.Helper()

	for _, text := range req.userInputTexts(t) {
		if strings.Contains(text, want) {
			return true
		}
	}
	return false
}

func assertContainsText(t *testing.T, texts []string, want string) {
	t.Helper()
	for _, text := range texts {
		if strings.Contains(text, want) {
			return
		}
	}
	t.Fatalf("texts %#v did not include %q", texts, want)
}

func assertDoesNotContainText(t *testing.T, texts []string, want string) {
	t.Helper()
	for _, text := range texts {
		if strings.Contains(text, want) {
			t.Fatalf("texts %#v unexpectedly included %q", texts, want)
		}
	}
}
