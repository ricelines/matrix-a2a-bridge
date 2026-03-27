package bridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"matrix-a2a-bridge/internal/a2a"
	bridgepkg "matrix-a2a-bridge/internal/bridge"
	"matrix-a2a-bridge/internal/config"
	"matrix-a2a-bridge/internal/state"
)

const (
	tuwunelImage       = "ghcr.io/matrix-construct/tuwunel:v1.5.1"
	tuwunelContainerPt = 6167
	serverName         = "tuwunel.test"
	aliceUser          = "alice"
	alicePassword      = "alice-password"
	botUser            = "bot"
	botPassword        = "bot-password"
	liveEnvVar         = "MATRIX_A2A_BRIDGE_RUN_LIVE"
)

const (
	eventForwardTimeout = 5 * time.Second
	noActivityWindow    = 1 * time.Second
	joinTimeout         = 5 * time.Second
)

var (
	liveSuiteOnce sync.Once
	liveSuiteInst *liveSuite
	liveSuiteErr  error
)

type forwardedNotification struct {
	Kind         string                      `json:"kind"`
	BridgeUserID string                      `json:"bridge_user_id"`
	RoomID       string                      `json:"room_id"`
	Updates      []forwardedNotificationPart `json:"updates"`
}

type forwardedNotificationPart struct {
	RoomSection string                     `json:"room_section"`
	State       []forwardedNotificationEvt `json:"state,omitempty"`
	Timeline    []forwardedNotificationEvt `json:"timeline,omitempty"`
}

type forwardedNotificationEvt struct {
	EventID  string         `json:"event_id,omitempty"`
	RoomID   string         `json:"room_id,omitempty"`
	Sender   string         `json:"sender,omitempty"`
	StateKey string         `json:"state_key,omitempty"`
	Type     string         `json:"type"`
	Content  map[string]any `json:"content"`
}

func TestMain(m *testing.M) {
	code := m.Run()

	if liveSuiteInst != nil {
		if err := liveSuiteInst.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "close live suite: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}

	os.Exit(code)
}

func TestBridgeForwardsInviteWithoutAutoJoining(t *testing.T) {
	suite := sharedLiveSuite(t)

	baseline := suite.upstream.NotificationCount()
	roomID := suite.createPrivateRoomWithBotInvite(t)

	notification := suite.waitForNotificationSince(t, baseline, eventForwardTimeout, func(n forwardedNotification) bool {
		return n.Kind == "matrix_room_update" &&
			n.BridgeUserID == suite.botUserID.String() &&
			n.RoomID == roomID.String() &&
			len(n.Updates) == 1 &&
			n.Updates[0].RoomSection == "invite" &&
			len(n.Updates[0].State) >= 2 &&
			hasMatchingForwardedEvent(n.Updates[0].State, func(evt forwardedNotificationEvt) bool {
				return evt.RoomID == roomID.String() &&
					evt.Type == event.StateMember.String() &&
					evt.StateKey == suite.botUserID.String() &&
					evt.Content["membership"] == "invite"
			})
	}, "invite event was not forwarded upstream")

	if notification.RoomID != roomID.String() {
		t.Fatalf("notification room_id = %q, want %q", notification.RoomID, roomID)
	}

	suite.assertNoBotJoinOrReply(t, roomID, noActivityWindow)
}

func TestBridgeForwardsJoinedRoomMessagesWithoutReplying(t *testing.T) {
	suite := sharedLiveSuite(t)

	roomID := suite.createPrivateRoomWithBotInvite(t)
	suite.joinRoomAsBot(t, roomID)
	waitForJoinedMember(t, suite.alice.client, roomID, suite.botUserID, joinTimeout)

	baseline := suite.upstream.NotificationCount()
	suite.alice.sendText(t, roomID, "hello from the room")

	notification := suite.waitForNotificationSince(t, baseline, eventForwardTimeout, func(n forwardedNotification) bool {
		return n.Kind == "matrix_room_update" &&
			n.RoomID == roomID.String() &&
			hasMatchingForwardedTimelineEvent(n, func(evt forwardedNotificationEvt) bool {
				return evt.RoomID == roomID.String() &&
					evt.Sender == suite.aliceUserID.String() &&
					evt.Type == event.EventMessage.String() &&
					evt.Content["body"] == "hello from the room"
			})
	}, "joined-room message was not forwarded upstream")

	eventBody, ok := firstMatchingForwardedTimelineEvent(notification, func(evt forwardedNotificationEvt) bool {
		return evt.Type == event.EventMessage.String() && evt.Content["body"] == "hello from the room"
	})
	if !ok || eventBody.Content["msgtype"] != "m.text" {
		t.Fatalf("notification content = %#v, want msgtype m.text", eventBody.Content)
	}

	suite.alice.waitForNoMessageFrom(t, roomID, suite.botUserID, noActivityWindow)
}

func TestBridgeIgnoresSelfAuthoredMessages(t *testing.T) {
	suite := sharedLiveSuite(t)

	roomID := suite.createPrivateRoomWithBotInvite(t)
	suite.joinRoomAsBot(t, roomID)
	waitForJoinedMember(t, suite.alice.client, roomID, suite.botUserID, joinTimeout)

	baseline := suite.upstream.NotificationCount()
	body := "message sent as the bridge account"
	suite.sendTextAsBot(t, roomID, body)
	suite.waitForNoMatchingNotificationSince(t, baseline, noActivityWindow, func(n forwardedNotification) bool {
		return n.Kind == "matrix_room_update" &&
			n.RoomID == roomID.String() &&
			hasMatchingForwardedTimelineEvent(n, func(evt forwardedNotificationEvt) bool {
				return evt.Sender == suite.botUserID.String() &&
					evt.Type == event.EventMessage.String() &&
					evt.Content["body"] == body
			})
	})
}

func TestBridgeMaintainsSeparateRoomHistoriesPerRoom(t *testing.T) {
	suite := sharedLiveSuite(t)

	roomA := suite.createPrivateRoomWithBotInvite(t)
	suite.joinRoomAsBot(t, roomA)
	waitForJoinedMember(t, suite.alice.client, roomA, suite.botUserID, joinTimeout)

	roomB := suite.createPrivateRoomWithBotInvite(t)
	suite.joinRoomAsBot(t, roomB)
	waitForJoinedMember(t, suite.alice.client, roomB, suite.botUserID, joinTimeout)

	baseline := suite.upstream.NotificationCount()

	suite.alice.sendText(t, roomA, "Hi. My name is Alice")
	firstA := suite.waitForRecordedNotificationSince(t, baseline, eventForwardTimeout, func(n a2a.RecordedNotification) bool {
		decoded := decodeNotification(t, n.Body)
		return decoded.RoomID == roomA.String() &&
			hasMatchingForwardedTimelineEvent(decoded, func(evt forwardedNotificationEvt) bool {
				return evt.Content["body"] == "Hi. My name is Alice"
			})
	}, "first room A message was not forwarded upstream")

	suite.alice.sendText(t, roomA, "What is my name?")
	secondA := suite.waitForRecordedNotificationSince(t, baseline, eventForwardTimeout, func(n a2a.RecordedNotification) bool {
		decoded := decodeNotification(t, n.Body)
		return decoded.RoomID == roomA.String() &&
			hasMatchingForwardedTimelineEvent(decoded, func(evt forwardedNotificationEvt) bool {
				return evt.Content["body"] == "What is my name?"
			})
	}, "second room A message was not forwarded upstream")

	suite.alice.sendText(t, roomB, "Hi. what is my name?")
	firstB := suite.waitForRecordedNotificationSince(t, baseline, eventForwardTimeout, func(n a2a.RecordedNotification) bool {
		decoded := decodeNotification(t, n.Body)
		return decoded.RoomID == roomB.String() &&
			hasMatchingForwardedTimelineEvent(decoded, func(evt forwardedNotificationEvt) bool {
				return evt.Content["body"] == "Hi. what is my name?"
			})
	}, "room B message was not forwarded upstream")

	if firstA.ContextID == "" {
		t.Fatal("room A first context id should not be empty")
	}
	assertHistoryUsesOnlyRoom(t, firstA.History, roomA)
	assertRoomMessageBodies(t, firstA.History, roomA, []string{"Hi. My name is Alice"})

	if secondA.ContextID != firstA.ContextID {
		t.Fatalf("room A second context id = %q, want %q", secondA.ContextID, firstA.ContextID)
	}
	if secondA.TaskID != firstA.TaskID {
		t.Fatalf("room A second task id = %q, want %q", secondA.TaskID, firstA.TaskID)
	}
	assertHistoryUsesOnlyRoom(t, secondA.History, roomA)
	assertRoomMessageBodies(t, secondA.History, roomA, []string{"Hi. My name is Alice", "What is my name?"})

	if firstB.ContextID == "" {
		t.Fatal("room B context id should not be empty")
	}
	if firstB.ContextID == firstA.ContextID {
		t.Fatalf("room B context id = %q, want distinct room-scoped context from room A", firstB.ContextID)
	}
	assertHistoryUsesOnlyRoom(t, firstB.History, roomB)
	assertRoomMessageBodies(t, firstB.History, roomB, []string{"Hi. what is my name?"})
}

type liveSuite struct {
	server      *tuwunelContainer
	upstream    *a2a.MockServer
	alice       *syncingClient
	botControl  *mautrix.Client
	bridgeRun   *runningBridge
	bridgeLog   *bytes.Buffer
	statePath   string
	botUserID   id.UserID
	aliceUserID id.UserID
}

func sharedLiveSuite(t *testing.T) *liveSuite {
	t.Helper()
	requireLiveBridgeTests(t)

	liveSuiteOnce.Do(func() {
		liveSuiteInst, liveSuiteErr = startLiveSuite()
	})
	if liveSuiteErr != nil {
		t.Fatalf("start live suite: %v", liveSuiteErr)
	}

	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf("bridge logs:\n%s", liveSuiteInst.bridgeLog.String())
		t.Logf("tuwunel logs:\n%s", liveSuiteInst.server.logs())
	})

	return liveSuiteInst
}

func startLiveSuite() (*liveSuite, error) {
	if testing.Short() {
		return nil, fmt.Errorf("live docker tests were explicitly requested in short mode")
	}
	if err := ensureDocker(); err != nil {
		return nil, err
	}

	server, err := startTuwunelContainer()
	if err != nil {
		return nil, err
	}

	upstream, err := startMockServer()
	if err != nil {
		_ = server.Close()
		return nil, err
	}

	alice, err := startSyncingClient(server.baseURL(), aliceUser, alicePassword)
	if err != nil {
		upstream.Close()
		_ = server.Close()
		return nil, err
	}

	botControl, err := newLoggedInClient(server.baseURL(), botUser, botPassword)
	if err != nil {
		_ = alice.Close()
		upstream.Close()
		_ = server.Close()
		return nil, err
	}

	bridgeLog := &bytes.Buffer{}
	statePath := filepath.Join(server.tempDir, "bridge-state.json")
	bridgeRun, err := startBridgeRuntime(server.baseURL(), upstream.BaseURL(), statePath, bridgeLog)
	if err != nil {
		_ = alice.Close()
		upstream.Close()
		_ = server.Close()
		return nil, err
	}

	suite := &liveSuite{
		server:      server,
		upstream:    upstream,
		alice:       alice,
		botControl:  botControl,
		bridgeRun:   bridgeRun,
		bridgeLog:   bridgeLog,
		statePath:   statePath,
		botUserID:   id.UserID(fmt.Sprintf("@%s:%s", botUser, serverName)),
		aliceUserID: id.UserID(fmt.Sprintf("@%s:%s", aliceUser, serverName)),
	}

	if err := suite.waitUntilReady(); err != nil {
		_ = suite.Close()
		return nil, err
	}

	return suite, nil
}

func (s *liveSuite) waitUntilReady() error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		store, err := state.Open(s.statePath)
		if err == nil {
			snapshot := store.Snapshot()
			if snapshot.Session.UserID == s.botUserID.String() && snapshot.Sync.NextBatch != "" {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("bridge did not finish initial sync")
}

func (s *liveSuite) Close() error {
	var errs []error

	if s.bridgeRun != nil {
		if err := s.bridgeRun.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.alice != nil {
		if err := s.alice.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.upstream != nil {
		s.upstream.Close()
	}
	if s.server != nil {
		if err := s.server.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (s *liveSuite) createPrivateRoomWithBotInvite(t *testing.T) id.RoomID {
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

func (s *liveSuite) joinRoomAsBot(t *testing.T, roomID id.RoomID) {
	t.Helper()
	if _, err := s.botControl.JoinRoomByID(context.Background(), roomID); err != nil {
		t.Fatalf("JoinRoomByID() error = %v", err)
	}
}

func (s *liveSuite) sendTextAsBot(t *testing.T, roomID id.RoomID, body string) {
	t.Helper()
	if _, err := s.botControl.SendText(context.Background(), roomID, body); err != nil {
		t.Fatalf("SendText(%q) error = %v", body, err)
	}
}

func (s *liveSuite) waitForNotificationSince(t *testing.T, start int, timeout time.Duration, predicate func(forwardedNotification) bool, message string) forwardedNotification {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		notifications := s.upstream.Notifications()
		for i := start; i < len(notifications); i++ {
			decoded := decodeNotification(t, notifications[i].Body)
			if predicate(decoded) {
				return decoded
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(message)
	return forwardedNotification{}
}

func (s *liveSuite) waitForNoMatchingNotificationSince(t *testing.T, start int, timeout time.Duration, predicate func(forwardedNotification) bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		notifications := s.upstream.Notifications()
		for i := start; i < len(notifications); i++ {
			decoded := decodeNotification(t, notifications[i].Body)
			if predicate(decoded) {
				t.Fatalf("unexpected matching notification: %+v", decoded)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *liveSuite) waitForRecordedNotificationSince(t *testing.T, start int, timeout time.Duration, predicate func(a2a.RecordedNotification) bool, message string) a2a.RecordedNotification {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		notifications := s.upstream.Notifications()
		for i := start; i < len(notifications); i++ {
			if predicate(notifications[i]) {
				return notifications[i]
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(message)
	return a2a.RecordedNotification{}
}

func (s *liveSuite) assertNoBotJoinOrReply(t *testing.T, roomID id.RoomID, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		members, err := s.alice.client.JoinedMembers(context.Background(), roomID)
		if err == nil {
			if _, ok := members.Joined[s.botUserID]; ok {
				t.Fatalf("unexpected joined member %s in room %s", s.botUserID, roomID)
			}
		}

		select {
		case evt := <-s.alice.messageEvents:
			if evt.RoomID == roomID && evt.Sender == s.botUserID {
				t.Fatalf("unexpected message from %s in room %s: %q", s.botUserID, roomID, evt.Content.AsMessage().Body)
			}
		case err := <-s.alice.done:
			t.Fatalf("sync client exited while waiting for silence: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func decodeNotification(t *testing.T, body string) forwardedNotification {
	t.Helper()

	var notification forwardedNotification
	if err := json.Unmarshal([]byte(body), &notification); err != nil {
		t.Fatalf("json.Unmarshal(notification) error = %v", err)
	}
	return notification
}

func assertHistoryUsesOnlyRoom(t *testing.T, history []string, roomID id.RoomID) {
	t.Helper()

	for _, entry := range history {
		decoded := decodeNotification(t, entry)
		if decoded.RoomID != roomID.String() {
			t.Fatalf("history contains event for room %q, want only %q: %#v", decoded.RoomID, roomID, history)
		}
	}
}

func assertRoomMessageBodies(t *testing.T, history []string, roomID id.RoomID, want []string) {
	t.Helper()

	got := roomMessageBodies(t, history, roomID)
	if !slices.Equal(got, want) {
		t.Fatalf("room %s message history = %#v, want %#v", roomID, got, want)
	}
}

func roomMessageBodies(t *testing.T, history []string, roomID id.RoomID) []string {
	t.Helper()

	bodies := make([]string, 0, len(history))
	for _, entry := range history {
		decoded := decodeNotification(t, entry)
		if decoded.RoomID != roomID.String() {
			continue
		}
		for _, update := range decoded.Updates {
			for _, evt := range update.Timeline {
				if evt.Type != event.EventMessage.String() {
					continue
				}
				body, _ := evt.Content["body"].(string)
				if body == "" {
					continue
				}
				bodies = append(bodies, body)
			}
		}
	}
	return bodies
}

func hasMatchingForwardedEvent(events []forwardedNotificationEvt, predicate func(forwardedNotificationEvt) bool) bool {
	for _, evt := range events {
		if predicate(evt) {
			return true
		}
	}
	return false
}

func hasMatchingForwardedTimelineEvent(notification forwardedNotification, predicate func(forwardedNotificationEvt) bool) bool {
	_, ok := firstMatchingForwardedTimelineEvent(notification, predicate)
	return ok
}

func firstMatchingForwardedTimelineEvent(notification forwardedNotification, predicate func(forwardedNotificationEvt) bool) (forwardedNotificationEvt, bool) {
	for _, update := range notification.Updates {
		for _, evt := range update.Timeline {
			if predicate(evt) {
				return evt, true
			}
		}
	}
	return forwardedNotificationEvt{}, false
}

type runningBridge struct {
	cancel context.CancelFunc
	done   chan error
}

func startBridgeRuntime(homeserverURL, upstreamURL, statePath string, logBuf *bytes.Buffer) (*runningBridge, error) {
	if _, err := state.Open(statePath); err != nil {
		return nil, fmt.Errorf("state.Open() error = %w", err)
	}

	cfg := config.Config{
		HomeserverURL:  homeserverURL,
		Username:       botUser,
		Password:       botPassword,
		StatePath:      statePath,
		UpstreamA2AURL: upstreamURL,
	}
	matrixBridge, err := bridgepkg.New(cfg, slog.New(slog.NewTextHandler(logBuf, nil)))
	if err != nil {
		return nil, fmt.Errorf("bridge.New() error = %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- matrixBridge.Run(ctx)
	}()

	return &runningBridge{
		cancel: cancel,
		done:   done,
	}, nil
}

func (r *runningBridge) Close() error {
	if r == nil {
		return nil
	}

	r.cancel()
	select {
	case err := <-r.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("bridge stopped with error: %w", err)
		}
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timed out waiting for bridge to stop")
	}
}

type tuwunelContainer struct {
	name     string
	tempDir  string
	hostPort string
}

func startTuwunelContainer() (*tuwunelContainer, error) {
	tempDir, err := tempDirInCurrentTree(".tmp-tuwunel-")
	if err != nil {
		return nil, err
	}

	hostPort, err := reserveHostPort()
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, err
	}

	container := &tuwunelContainer{
		name:     fmt.Sprintf("matrix-a2a-bridge-tuwunel-%d", time.Now().UnixNano()),
		tempDir:  tempDir,
		hostPort: hostPort,
	}
	if err := container.start(); err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, err
	}
	return container, nil
}

func (c *tuwunelContainer) start() error {
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
		"--execute", fmt.Sprintf("users create_user %s %s", aliceUser, alicePassword),
		"--execute", fmt.Sprintf("users create_user %s %s", botUser, botPassword),
	}

	if output, err := runCommand(5*time.Minute, "docker", args...); err != nil {
		return fmt.Errorf("docker run failed: %v\n%s", err, output)
	}

	if err := c.waitReady(); err != nil {
		return err
	}
	return nil
}

func (c *tuwunelContainer) Close() error {
	var errs []error

	if c.hostPort != "" {
		if output, err := runCommand(30*time.Second, "docker", "stop", "-t", "1", c.name); err != nil {
			errs = append(errs, fmt.Errorf("docker stop failed: %v\n%s", err, output))
		}
	}
	if c.tempDir != "" {
		if err := os.RemoveAll(c.tempDir); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (c *tuwunelContainer) waitReady() error {
	deadline := time.Now().Add(60 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	url := c.baseURL() + "/_matrix/client/versions"
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(250 * time.Millisecond)
	}

	return fmt.Errorf("timed out waiting for tuwunel to become ready at %s", url)
}

func (c *tuwunelContainer) baseURL() string {
	return "http://127.0.0.1:" + c.hostPort
}

func (c *tuwunelContainer) logs() string {
	output, _ := runCommand(20*time.Second, "docker", "logs", c.name)
	return output
}

type syncingClient struct {
	client        *mautrix.Client
	messageEvents chan *event.Event
	cancel        context.CancelFunc
	done          chan error
}

func startSyncingClient(homeserverURL, username, password string) (*syncingClient, error) {
	client, err := newLoggedInClient(homeserverURL, username, password)
	if err != nil {
		return nil, err
	}

	sc := &syncingClient{
		client:        client,
		messageEvents: make(chan *event.Event, 256),
		done:          make(chan error, 1),
	}

	syncer := client.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		sc.messageEvents <- evt
	})

	ctx, cancel := context.WithCancel(context.Background())
	sc.cancel = cancel
	go func() {
		sc.done <- client.SyncWithContext(ctx)
	}()

	return sc, nil
}

func (c *syncingClient) Close() error {
	if c == nil {
		return nil
	}

	c.cancel()
	select {
	case err := <-c.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("sync client stopped with error: %w", err)
		}
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timed out stopping sync client")
	}
}

func newLoggedInClient(homeserverURL, username, password string) (*mautrix.Client, error) {
	client, err := mautrix.NewClient(homeserverURL, "", "")
	if err != nil {
		return nil, fmt.Errorf("NewClient() error = %w", err)
	}
	client.DefaultHTTPRetries = 3
	client.DefaultHTTPBackoff = 2 * time.Second

	deadline := time.Now().Add(10 * time.Second)
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
			return client, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("Login() error = %w", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (c *syncingClient) sendText(t *testing.T, roomID id.RoomID, body string) {
	t.Helper()
	if _, err := c.client.SendText(context.Background(), roomID, body); err != nil {
		t.Fatalf("SendText(%q) error = %v", body, err)
	}
}

func waitForJoinedMember(t *testing.T, client *mautrix.Client, roomID id.RoomID, userID id.UserID, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		members, err := client.JoinedMembers(context.Background(), roomID)
		if err == nil {
			if _, ok := members.Joined[userID]; ok {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for join membership for %s in room %s", userID, roomID)
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

func ensureDocker() error {
	output, err := runCommand(20*time.Second, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return fmt.Errorf("docker is required for live tuwunel tests: %v\n%s", err, output)
	}
	return nil
}

func requireLiveBridgeTests(t *testing.T) {
	t.Helper()
	if os.Getenv(liveEnvVar) == "" {
		t.Skipf("set %s=1 to run live bridge tests", liveEnvVar)
	}
}

func tempDirInCurrentTree(pattern string) (string, error) {
	dir, err := os.MkdirTemp(".", pattern)
	if err != nil {
		return "", fmt.Errorf("MkdirTemp() error = %w", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("filepath.Abs() error = %w", err)
	}
	return absDir, nil
}

func reserveHostPort() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("Listen() error = %w", err)
	}
	defer listener.Close()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return "", fmt.Errorf("unexpected listener address type %T", listener.Addr())
	}
	return fmt.Sprintf("%d", addr.Port), nil
}

func startMockServer() (server *a2a.MockServer, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("start mock A2A server: %v", r)
		}
	}()
	return a2a.StartMockServer(), nil
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
