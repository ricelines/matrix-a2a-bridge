package bot_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"onboarding/internal/agent"
	botpkg "onboarding/internal/bot"
	"onboarding/internal/config"
	"onboarding/internal/state"
)

const (
	tuwunelImage       = "ghcr.io/matrix-construct/tuwunel:v1.5.1"
	tuwunelContainerPt = 6167
	serverName         = "tuwunel.test"
	aliceUser          = "alice"
	alicePassword      = "alice-password"
	botUser            = "bot"
	botPassword        = "bot-password"
)

func TestBotStartsOnboardingOnFirstDirectInvite(t *testing.T) {
	harness := newLiveHarness(t)
	harness.startBot(t)

	roomID := harness.createDirectRoomWithBot(t)
	harness.alice.waitForMessage(t, roomID, harness.botUserID, "Welcome to Ricelines. I'll get you oriented. What should I call you?", 20*time.Second)

	harness.alice.sendText(t, roomID, "Call me Alice")
	harness.alice.waitForMessage(t, roomID, harness.botUserID, "What brings you to Ricelines today?", 20*time.Second)

	harness.alice.sendText(t, roomID, "I'm here to explore")
	harness.alice.waitForMessage(t, roomID, harness.botUserID, "Thanks. You're onboarded now, and you can keep chatting with me naturally whenever you need help.", 20*time.Second)
}

func TestBotOnboardsOnlyOnceAcrossRestart(t *testing.T) {
	harness := newLiveHarness(t)
	harness.startBot(t)

	firstRoomID := harness.createDirectRoomWithBot(t)
	harness.completeOnboarding(t, firstRoomID)
	harness.waitForOnboardingRecord(t)

	harness.stopBot(t)
	harness.startBot(t)

	secondRoomID := harness.createDirectRoomWithBot(t)
	harness.alice.waitForNoMessageFrom(t, secondRoomID, harness.botUserID, 5*time.Second)

	harness.alice.sendText(t, secondRoomID, "Can you show my status?")
	harness.alice.waitForMessageContaining(t, secondRoomID, harness.botUserID, "Your onboarding record is set, and this conversation is using the shared A2A context.", 20*time.Second)
}

func TestBotStartsNewTaskAfterIdleTimeout(t *testing.T) {
	harness := newLiveHarness(t)
	harness.startBot(t)

	roomID := harness.createDirectRoomWithBot(t)
	harness.completeOnboarding(t, roomID)

	harness.alice.sendText(t, roomID, "What can you do?")
	harness.alice.waitForMessageContaining(t, roomID, harness.botUserID, "I can keep this DM connected to an A2A task, preserve shared context, and continue the conversation naturally.", 20*time.Second)

	waitForCondition(t, 10*time.Second, func() bool {
		return harness.agent.TaskCountForRoom(roomID.String()) == 2
	}, "first post-onboarding task was not created")

	taskIDs := harness.agent.TaskIDsForRoom(roomID.String())
	if len(taskIDs) != 2 {
		t.Fatalf("unexpected task IDs after first post-onboarding message: %v", taskIDs)
	}
	firstGeneralTaskID := taskIDs[1]

	waitForCondition(t, harness.sessionIdleTimeout+5*time.Second, func() bool {
		return harness.agent.WasTaskCanceled(firstGeneralTaskID)
	}, "idle session task was not canceled")

	harness.alice.sendText(t, roomID, "status please")
	harness.alice.waitForMessageContaining(t, roomID, harness.botUserID, "Your onboarding record is set, and this conversation is using the shared A2A context.", 20*time.Second)

	waitForCondition(t, 10*time.Second, func() bool {
		return harness.agent.TaskCountForRoom(roomID.String()) == 3
	}, "second post-idle task was not created")

	if got := harness.agent.ContextCountForUser(harness.aliceUserID.String()); got != 1 {
		t.Fatalf("ContextCountForUser() = %d, want 1 shared context", got)
	}
}

type liveHarness struct {
	t                  *testing.T
	server             *tuwunelContainer
	agent              *agent.MockServer
	alice              *syncingClient
	botRun             *runningBot
	botLog             *bytes.Buffer
	statePath          string
	botUserID          id.UserID
	aliceUserID        id.UserID
	sessionIdleTimeout time.Duration
}

func newLiveHarness(t *testing.T) *liveHarness {
	t.Helper()
	requireDocker(t)

	server := newTuwunelContainer(t)
	agentServer := agent.StartMockServer()
	alice := newSyncingClient(t, server.baseURL(), aliceUser, alicePassword)

	t.Cleanup(agentServer.Close)

	return &liveHarness{
		t:                  t,
		server:             server,
		agent:              agentServer,
		alice:              alice,
		botLog:             &bytes.Buffer{},
		statePath:          filepath.Join(server.tempDir, "bot-state.json"),
		botUserID:          id.UserID(fmt.Sprintf("@%s:%s", botUser, serverName)),
		aliceUserID:        id.UserID(fmt.Sprintf("@%s:%s", aliceUser, serverName)),
		sessionIdleTimeout: 2 * time.Second,
	}
}

func (h *liveHarness) startBot(t *testing.T) {
	t.Helper()
	if h.botRun != nil {
		t.Fatalf("bot already running")
	}

	waitForPasswordLogin(t, h.server.baseURL(), botUser, botPassword)
	existingState, err := state.Open(h.statePath)
	if err != nil {
		t.Fatalf("state.Open() error = %v", err)
	}
	hadSyncCursor := existingState.Snapshot().Sync.NextBatch != ""

	cfg := config.Config{
		HomeserverURL:      h.server.baseURL(),
		Username:           botUser,
		Password:           botPassword,
		AutoJoinInvites:    true,
		StatePath:          h.statePath,
		A2AAgentURL:        h.agent.BaseURL(),
		SessionIdleTimeout: h.sessionIdleTimeout,
	}
	matrixBot, err := botpkg.New(cfg, slog.New(slog.NewTextHandler(h.botLog, nil)))
	if err != nil {
		t.Fatalf("bot.New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- matrixBot.Run(ctx)
	}()

	h.botRun = &runningBot{
		cancel: cancel,
		done:   done,
	}

	t.Cleanup(func() {
		if h.botRun != nil {
			h.stopBot(t)
		}
	})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("bot logs:\n%s", h.botLog.String())
		}
	})

	waitForCondition(t, 20*time.Second, func() bool {
		store, err := state.Open(h.statePath)
		if err != nil {
			return false
		}
		snapshot := store.Snapshot()
		return snapshot.Session.UserID == h.botUserID.String() && snapshot.Sync.NextBatch != ""
	}, "bot did not finish initial sync")
	if hadSyncCursor {
		time.Sleep(500 * time.Millisecond)
	}
}

func (h *liveHarness) stopBot(t *testing.T) {
	t.Helper()
	if h.botRun == nil {
		return
	}

	h.botRun.cancel()
	select {
	case err := <-h.botRun.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("bot stopped with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for bot to stop")
	}
	h.botRun = nil
}

func (h *liveHarness) createDirectRoomWithBot(t *testing.T) id.RoomID {
	t.Helper()

	room, err := h.alice.client.CreateRoom(context.Background(), &mautrix.ReqCreateRoom{
		Preset:   "private_chat",
		IsDirect: true,
	})
	if err != nil {
		t.Fatalf("CreateRoom() error = %v", err)
	}

	if _, err := h.alice.client.InviteUser(context.Background(), room.RoomID, &mautrix.ReqInviteUser{
		UserID: h.botUserID,
	}); err != nil {
		t.Fatalf("InviteUser() error = %v", err)
	}

	h.alice.waitForMembership(t, room.RoomID, h.botUserID, event.MembershipJoin, 20*time.Second)
	return room.RoomID
}

func (h *liveHarness) completeOnboarding(t *testing.T, roomID id.RoomID) {
	t.Helper()

	h.alice.waitForMessage(t, roomID, h.botUserID, "Welcome to Ricelines. I'll get you oriented. What should I call you?", 20*time.Second)
	h.alice.sendText(t, roomID, "Alice")
	h.alice.waitForMessage(t, roomID, h.botUserID, "What brings you to Ricelines today?", 20*time.Second)
	h.alice.sendText(t, roomID, "I want to learn the system")
	h.alice.waitForMessage(t, roomID, h.botUserID, "Thanks. You're onboarded now, and you can keep chatting with me naturally whenever you need help.", 20*time.Second)
}

func (h *liveHarness) waitForOnboardingRecord(t *testing.T) {
	t.Helper()

	client := newLoggedInClient(t, h.server.baseURL(), botUser, botPassword)
	eventType := onboardingBucketEventType(h.aliceUserID)

	waitForCondition(t, 10*time.Second, func() bool {
		var bucket struct {
			Users map[string]struct {
				ContextID   string     `json:"context_id,omitempty"`
				OnboardedAt *time.Time `json:"onboarded_at,omitempty"`
			} `json:"users,omitempty"`
		}
		if err := client.GetAccountData(context.Background(), eventType, &bucket); err != nil {
			return false
		}

		record, ok := bucket.Users[h.aliceUserID.String()]
		return ok && record.OnboardedAt != nil && record.ContextID != ""
	}, "bot did not persist onboarding record")
}

type runningBot struct {
	cancel context.CancelFunc
	done   chan error
}

type tuwunelContainer struct {
	t            *testing.T
	name         string
	tempDir      string
	hostPort     string
	bootstrapped bool
}

func newTuwunelContainer(t *testing.T) *tuwunelContainer {
	t.Helper()

	tempDir := mustTempDirInCurrentTree(t, ".tmp-tuwunel-")
	container := &tuwunelContainer{
		t:        t,
		name:     fmt.Sprintf("onboarding-tuwunel-%d", time.Now().UnixNano()),
		tempDir:  tempDir,
		hostPort: reserveHostPort(t),
	}
	container.start(t)

	t.Cleanup(func() {
		container.stop(t)
		_ = os.RemoveAll(tempDir)
	})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("tuwunel logs:\n%s", container.logs(t))
		}
	})

	return container
}

func (c *tuwunelContainer) start(t *testing.T) {
	t.Helper()

	args := []string{
		"run", "--rm", "-d",
		"--name", c.name,
		"-p", fmt.Sprintf("127.0.0.1:%s:%d", c.hostPort, tuwunelContainerPt),
		"-v", fmt.Sprintf("%s:/var/lib/tuwunel", c.tempDir),
		"-e", fmt.Sprintf("TUWUNEL_SERVER_NAME=%s", serverName),
		"-e", "TUWUNEL_DATABASE_PATH=/var/lib/tuwunel",
		"-e", fmt.Sprintf("TUWUNEL_PORT=%d", tuwunelContainerPt),
		"-e", "TUWUNEL_ADDRESS=0.0.0.0",
		"-e", "TUWUNEL_ALLOW_FEDERATION=false",
		tuwunelImage,
	}
	if !c.bootstrapped {
		args = append(args,
			"--execute", fmt.Sprintf("users create_user %s %s", aliceUser, alicePassword),
			"--execute", fmt.Sprintf("users create_user %s %s", botUser, botPassword),
		)
	}

	if output, err := runCommand(5*time.Minute, "docker", args...); err != nil {
		t.Fatalf("docker run failed: %v\n%s", err, output)
	}

	c.bootstrapped = true
	c.waitReady(t)
}

func (c *tuwunelContainer) stop(t *testing.T) {
	t.Helper()
	if c.hostPort == "" {
		return
	}

	_, _ = runCommand(30*time.Second, "docker", "stop", "-t", "1", c.name)
}

func (c *tuwunelContainer) waitReady(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	url := c.baseURL() + "/_matrix/client/versions"
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for tuwunel to become ready at %s", url)
}

func (c *tuwunelContainer) baseURL() string {
	return "http://127.0.0.1:" + c.hostPort
}

func (c *tuwunelContainer) logs(t *testing.T) string {
	t.Helper()
	output, _ := runCommand(20*time.Second, "docker", "logs", c.name)
	return output
}

type syncingClient struct {
	t             *testing.T
	client        *mautrix.Client
	messageEvents chan *event.Event
	memberEvents  chan *event.Event
	cancel        context.CancelFunc
	done          chan error
}

func newSyncingClient(t *testing.T, homeserverURL, username, password string) *syncingClient {
	t.Helper()

	client := newLoggedInClient(t, homeserverURL, username, password)

	sc := &syncingClient{
		t:             t,
		client:        client,
		messageEvents: make(chan *event.Event, 256),
		memberEvents:  make(chan *event.Event, 256),
		done:          make(chan error, 1),
	}

	syncer := client.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		sc.messageEvents <- evt
	})
	syncer.OnEventType(event.StateMember, func(ctx context.Context, evt *event.Event) {
		sc.memberEvents <- evt
	})

	ctx, cancel := context.WithCancel(context.Background())
	sc.cancel = cancel
	go func() {
		sc.done <- client.SyncWithContext(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-sc.done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("sync client stopped with error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out stopping sync client")
		}
	})

	return sc
}

func waitForPasswordLogin(t *testing.T, homeserverURL, username, password string) {
	t.Helper()

	_ = newLoggedInClient(t, homeserverURL, username, password)
}

func newLoggedInClient(t *testing.T, homeserverURL, username, password string) *mautrix.Client {
	t.Helper()

	client, err := mautrix.NewClient(homeserverURL, "", "")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	client.DefaultHTTPRetries = 3
	client.DefaultHTTPBackoff = 2 * time.Second

	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err = client.Login(context.Background(), &mautrix.ReqLogin{
			Type: mautrix.AuthTypePassword,
			Identifier: mautrix.UserIdentifier{
				Type: mautrix.IdentifierTypeUser,
				User: username,
			},
			Password:         password,
			StoreCredentials: true,
		})
		if err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("Login() error = %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	return client
}

func onboardingBucketEventType(userID id.UserID) string {
	sum := sha256.Sum256([]byte(userID))
	return fmt.Sprintf("com.ricelines.onboarding.users.%02x", sum[0])
}

func (c *syncingClient) sendText(t *testing.T, roomID id.RoomID, body string) {
	t.Helper()
	if _, err := c.client.SendText(context.Background(), roomID, body); err != nil {
		t.Fatalf("SendText(%q) error = %v", body, err)
	}
}

func (c *syncingClient) waitForMembership(t *testing.T, roomID id.RoomID, userID id.UserID, membership event.Membership, timeout time.Duration) *event.Event {
	t.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case evt := <-c.memberEvents:
			if evt.RoomID != roomID {
				continue
			}
			if evt.GetStateKey() != userID.String() {
				continue
			}
			if evt.Content.AsMember().Membership != membership {
				continue
			}
			return evt
		case err := <-c.done:
			t.Fatalf("sync client exited while waiting for membership: %v", err)
		case <-timer.C:
			t.Fatalf("timed out waiting for %s membership for %s in room %s", membership, userID, roomID)
		}
	}
}

func (c *syncingClient) waitForMessage(t *testing.T, roomID id.RoomID, sender id.UserID, body string, timeout time.Duration) *event.Event {
	t.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case evt := <-c.messageEvents:
			if evt.RoomID != roomID || evt.Sender != sender {
				continue
			}
			if evt.Content.AsMessage().Body != body {
				continue
			}
			return evt
		case err := <-c.done:
			t.Fatalf("sync client exited while waiting for message: %v", err)
		case <-timer.C:
			t.Fatalf("timed out waiting for message %q from %s in room %s", body, sender, roomID)
		}
	}
}

func (c *syncingClient) waitForMessageContaining(t *testing.T, roomID id.RoomID, sender id.UserID, wantSubstring string, timeout time.Duration) *event.Event {
	t.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case evt := <-c.messageEvents:
			if evt.RoomID != roomID || evt.Sender != sender {
				continue
			}
			if !strings.Contains(evt.Content.AsMessage().Body, wantSubstring) {
				continue
			}
			return evt
		case err := <-c.done:
			t.Fatalf("sync client exited while waiting for message containing %q: %v", wantSubstring, err)
		case <-timer.C:
			t.Fatalf("timed out waiting for message containing %q from %s in room %s", wantSubstring, sender, roomID)
		}
	}
}

func (c *syncingClient) waitForNoMessageFrom(t *testing.T, roomID id.RoomID, sender id.UserID, timeout time.Duration) {
	t.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case evt := <-c.messageEvents:
			if evt.RoomID != roomID || evt.Sender != sender {
				continue
			}
			t.Fatalf("unexpected message from %s in room %s: %q", sender, roomID, evt.Content.AsMessage().Body)
		case err := <-c.done:
			t.Fatalf("sync client exited while waiting for silence: %v", err)
		case <-timer.C:
			return
		}
	}
}

func requireDocker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping live docker tests in short mode")
	}
	if output, err := runCommand(20*time.Second, "docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		t.Skipf("docker is required for live tuwunel tests: %v\n%s", err, output)
	}
}

func mustTempDirInCurrentTree(t *testing.T, pattern string) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", pattern)
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}
	return absDir
}

func reserveHostPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", listener.Addr())
	}
	return fmt.Sprintf("%d", addr.Port)
}

func runCommand(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(output), ctx.Err()
	}
	return string(output), err
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal(message)
}
