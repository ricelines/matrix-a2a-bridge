package bot_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	botpkg "onboarding/internal/bot"
	"onboarding/internal/config"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
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

func TestBotRepliesToLiveCommand(t *testing.T) {
	harness := newLiveHarness(t)
	harness.startBot(t)

	roomID := harness.createRoomAndInviteBot(t)
	harness.alice.sendText(t, roomID, "!ping")
	harness.alice.waitForMessage(t, roomID, harness.botUserID, "pong", 20*time.Second)
}

func TestBotReplaysMessagesAfterBotRestart(t *testing.T) {
	harness := newLiveHarness(t)
	harness.startBot(t)

	roomID := harness.createRoomAndInviteBot(t)
	harness.stopBot(t)

	harness.alice.sendText(t, roomID, "!echo first after restart")
	harness.alice.sendText(t, roomID, "!echo second after restart")

	harness.startBot(t)

	harness.alice.waitForMessage(t, roomID, harness.botUserID, "first after restart", 25*time.Second)
	harness.alice.waitForMessage(t, roomID, harness.botUserID, "second after restart", 25*time.Second)
}

func TestBotRecoversAfterHomeserverRestart(t *testing.T) {
	harness := newLiveHarness(t)
	harness.startBot(t)

	roomID := harness.createRoomAndInviteBot(t)
	harness.server.restart(t)

	harness.alice.sendText(t, roomID, "!echo after homeserver restart")
	harness.alice.waitForMessage(t, roomID, harness.botUserID, "after homeserver restart", 30*time.Second)
}

type liveHarness struct {
	t         *testing.T
	server    *tuwunelContainer
	alice     *syncingClient
	botRun    *runningBot
	statePath string
	botUserID id.UserID
}

func newLiveHarness(t *testing.T) *liveHarness {
	t.Helper()
	requireDocker(t)

	server := newTuwunelContainer(t)
	alice := newSyncingClient(t, server.baseURL(), aliceUser, alicePassword)

	return &liveHarness{
		t:         t,
		server:    server,
		alice:     alice,
		statePath: filepath.Join(server.tempDir, "bot-state.json"),
		botUserID: id.UserID(fmt.Sprintf("@%s:%s", botUser, serverName)),
	}
}

func (h *liveHarness) startBot(t *testing.T) {
	t.Helper()
	if h.botRun != nil {
		t.Fatalf("bot already running")
	}

	cfg := config.Config{
		HomeserverURL:   h.server.baseURL(),
		Username:        botUser,
		Password:        botPassword,
		CommandPrefix:   "!",
		AutoJoinInvites: true,
		StatePath:       h.statePath,
	}
	matrixBot, err := botpkg.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func (h *liveHarness) createRoomAndInviteBot(t *testing.T) id.RoomID {
	t.Helper()
	room, err := h.alice.client.CreateRoom(context.Background(), &mautrix.ReqCreateRoom{
		Preset: "private_chat",
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

func (c *tuwunelContainer) restart(t *testing.T) {
	t.Helper()
	c.stop(t)
	c.start(t)
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
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Login() error = %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

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
