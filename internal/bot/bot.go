package bot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	a2aproto "github.com/a2aproject/a2a-go/a2a"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"onboarding/internal/a2a"
	"onboarding/internal/config"
	"onboarding/internal/state"
)

type Bot struct {
	client   *mautrix.Client
	config   config.Config
	log      *slog.Logger
	state    *state.Store
	upstream *a2a.Client
	users    *userDirectory
	sessions *sessionManager

	roomPeersMu sync.Mutex
	roomPeers   map[id.RoomID]id.UserID
}

func New(cfg config.Config, logger *slog.Logger) (*Bot, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	stateStore, err := state.Open(cfg.StatePath)
	if err != nil {
		return nil, fmt.Errorf("open state store: %w", err)
	}

	initialSession, err := resolveInitialSession(cfg, stateStore.Snapshot())
	if err != nil {
		return nil, err
	}

	client, err := mautrix.NewClient(cfg.HomeserverURL, id.UserID(initialSession.UserID), initialSession.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("create matrix client: %w", err)
	}
	client.DeviceID = id.DeviceID(initialSession.DeviceID)
	client.DefaultHTTPRetries = 3
	client.DefaultHTTPBackoff = 2 * time.Second

	bot := &Bot{
		client:    client,
		config:    cfg,
		log:       logger,
		state:     stateStore,
		users:     newUserDirectory(client),
		sessions:  newSessionManager(cfg.SessionIdleTimeout),
		roomPeers: make(map[id.RoomID]id.UserID),
	}
	bot.registerHandlers()
	return bot, nil
}

func (b *Bot) Run(ctx context.Context) error {
	if err := b.authenticate(ctx); err != nil {
		return err
	}

	upstreamClient, err := a2a.New(ctx, b.config.UpstreamA2AURL)
	if err != nil {
		return fmt.Errorf("connect to upstream A2A endpoint: %w", err)
	}
	b.upstream = upstreamClient

	go b.runSessionReaper(ctx)

	b.log.Info("starting onboarding-agent Matrix runtime",
		"user_id", b.client.UserID.String(),
		"homeserver", b.client.HomeserverURL.String(),
		"state_path", b.config.StatePath,
		"upstream_a2a_url", b.config.UpstreamA2AURL,
		"session_idle_timeout", b.config.SessionIdleTimeout.String(),
	)

	for {
		err := b.syncWithResume(ctx)
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}
		if !errors.Is(err, mautrix.MUnknownToken) || !b.canRelogin() {
			return err
		}

		b.log.Warn("matrix session expired, attempting to re-authenticate")
		if err := b.loginWithPassword(ctx); err != nil {
			return fmt.Errorf("re-authenticate after unknown token: %w", err)
		}
	}
}

func (b *Bot) authenticate(ctx context.Context) error {
	if b.client.AccessToken != "" && b.client.UserID != "" {
		if err := b.persistSession(); err != nil {
			return err
		}
		return nil
	}

	return b.loginWithPassword(ctx)
}

func (b *Bot) loginWithPassword(ctx context.Context) error {
	loginReq := &mautrix.ReqLogin{
		Type: mautrix.AuthTypePassword,
		Identifier: mautrix.UserIdentifier{
			Type: mautrix.IdentifierTypeUser,
			User: usernameForLogin(b.config.Username),
		},
		Password:                 b.config.Password,
		InitialDeviceDisplayName: "onboarding-agent",
		StoreCredentials:         true,
	}
	if b.client.DeviceID != "" {
		loginReq.DeviceID = b.client.DeviceID
	}

	resp, err := b.client.Login(ctx, loginReq)
	if err != nil {
		return fmt.Errorf("log in with password: %w", err)
	}
	if err := b.persistSession(); err != nil {
		return err
	}

	b.log.Info("logged in to matrix",
		"user_id", resp.UserID.String(),
		"device_id", resp.DeviceID.String(),
	)

	return nil
}

func usernameForLogin(input string) string {
	if strings.HasPrefix(input, "@") {
		parsedUserID := id.UserID(input)
		if localpart, _, err := parsedUserID.Parse(); err == nil {
			return localpart
		}
	}
	return input
}

func (b *Bot) registerHandlers() {
	syncer := b.client.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEventType(event.StateMember, b.handleMemberEvent)
	syncer.OnEventType(event.EventMessage, b.handleMessageEvent)
}

func (b *Bot) handleMemberEvent(ctx context.Context, evt *event.Event) {
	if evt.GetStateKey() != b.client.UserID.String() {
		return
	}
	if evt.ID != "" && b.state.IsHandled(evt.ID.String()) {
		return
	}

	member := evt.Content.AsMember()
	switch member.Membership {
	case event.MembershipInvite:
		if _, err := b.client.JoinRoomByID(ctx, evt.RoomID); err != nil {
			b.log.Error("failed to join invited room",
				"room_id", evt.RoomID.String(),
				"sender", evt.Sender.String(),
				"err", err,
			)
			return
		}

		b.rememberRoomPeer(evt.RoomID, evt.Sender)
		_ = b.markHandledEvent(evt.ID.String(), "member")
	case event.MembershipJoin:
		if evt.Sender != b.client.UserID {
			return
		}
		_ = b.markHandledEvent(evt.ID.String(), "member")
	default:
		return
	}
}

func (b *Bot) handleMessageEvent(ctx context.Context, evt *event.Event) {
	if evt.Sender == b.client.UserID {
		return
	}
	if evt.ID != "" && b.state.IsHandled(evt.ID.String()) {
		return
	}

	message := evt.Content.AsMessage()
	if message.MsgType != event.MsgText && message.MsgType != event.MsgNotice {
		return
	}

	body := strings.TrimSpace(message.Body)
	if body == "" {
		return
	}

	userID, ok, err := b.resolveDirectPeer(ctx, evt.RoomID)
	if err != nil {
		b.log.Error("failed to resolve direct-message peer",
			"room_id", evt.RoomID.String(),
			"event_id", evt.ID.String(),
			"err", err,
		)
		if b.replyWithFailure(ctx, evt.RoomID, stableTransactionID("message-failure", evt.ID.String())) == nil {
			_ = b.markHandledEvent(evt.ID.String(), "message")
		}
		return
	}
	if !ok {
		return
	}

	record, err := b.users.Load(ctx, userID)
	if err != nil {
		b.log.Error("failed to load user onboarding record",
			"room_id", evt.RoomID.String(),
			"user_id", userID.String(),
			"err", err,
		)
		if b.replyWithFailure(ctx, evt.RoomID, stableTransactionID("message-failure", evt.ID.String())) == nil {
			_ = b.markHandledEvent(evt.ID.String(), "message")
		}
		return
	}

	now := time.Now().UTC()
	if expired, ok := b.sessions.ExpireRoom(evt.RoomID, now); ok {
		b.cancelSession(ctx, expired)
	}

	req := a2a.Request{
		Text:      body,
		ContextID: record.ContextID,
		Metadata:  requestMetadata(evt.RoomID, userID, conversationMode(record), "message"),
	}
	if current, ok := b.sessions.Active(evt.RoomID, now); ok {
		req.TaskID = current.TaskID
		if current.ContextID != "" {
			req.ContextID = current.ContextID
		}
	}

	reply, err := b.upstream.Send(ctx, req)
	if err != nil {
		b.log.Error("failed to route message through upstream A2A",
			"room_id", evt.RoomID.String(),
			"user_id", userID.String(),
			"event_id", evt.ID.String(),
			"err", err,
		)
		if b.replyWithFailure(ctx, evt.RoomID, stableTransactionID("message-failure", evt.ID.String())) == nil {
			_ = b.markHandledEvent(evt.ID.String(), "message")
		}
		return
	}

	replyBody := b.renderReply(reply)
	if replyBody == "" {
		replyBody = "I'm still here, but I didn't get a usable reply from the upstream A2A endpoint."
	}
	if err := b.sendText(ctx, evt.RoomID, stableTransactionID("message-reply", evt.ID.String()), replyBody); err != nil {
		b.log.Error("failed to send chat reply",
			"room_id", evt.RoomID.String(),
			"user_id", userID.String(),
			"event_id", evt.ID.String(),
			"err", err,
		)
		return
	}

	if err := b.persistReplyState(ctx, evt.RoomID, userID, record, reply, now); err != nil {
		b.log.Error("failed to persist conversation state",
			"room_id", evt.RoomID.String(),
			"user_id", userID.String(),
			"event_id", evt.ID.String(),
			"err", err,
		)
		return
	}

	_ = b.markHandledEvent(evt.ID.String(), "message")
}

func (b *Bot) renderReply(reply a2a.Response) string {
	return strings.TrimSpace(reply.Reply)
}

func (b *Bot) persistReplyState(ctx context.Context, roomID id.RoomID, userID id.UserID, record userRecord, reply a2a.Response, now time.Time) error {
	updated := record
	changed := false

	if reply.ContextID != "" && reply.ContextID != updated.ContextID {
		updated.ContextID = reply.ContextID
		changed = true
	}
	if !updated.Onboarded() && conversationMode(record) == "onboarding" && reply.State == a2aproto.TaskStateCompleted {
		timestamp := now
		updated.OnboardedAt = &timestamp
		changed = true
	}

	if changed {
		if err := b.users.Save(ctx, userID, updated); err != nil {
			return err
		}
	}

	if reply.TaskID == "" {
		b.sessions.Remove(roomID)
		return nil
	}
	switch reply.State {
	case a2aproto.TaskStateInputRequired, a2aproto.TaskStateAuthRequired:
	default:
		b.sessions.Remove(roomID)
		return nil
	}

	contextID := updated.ContextID
	if contextID == "" {
		contextID = reply.ContextID
	}
	b.sessions.Put(session{
		RoomID:       roomID,
		UserID:       userID,
		TaskID:       reply.TaskID,
		ContextID:    contextID,
		LastActivity: now,
	})
	return nil
}

func (b *Bot) runSessionReaper(ctx context.Context) {
	tickerInterval := minDuration(30*time.Second, maxDuration(time.Second, b.config.SessionIdleTimeout/2))
	ticker := time.NewTicker(tickerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, expired := range b.sessions.Expired(time.Now().UTC()) {
				b.cancelSession(ctx, expired)
			}
		}
	}
}

func (b *Bot) cancelSession(ctx context.Context, current session) {
	if current.TaskID == "" {
		return
	}
	if err := b.upstream.CancelTask(ctx, current.TaskID); err != nil {
		b.log.Warn("failed to cancel expired A2A task",
			"room_id", current.RoomID.String(),
			"user_id", current.UserID.String(),
			"task_id", current.TaskID,
			"err", err,
		)
	}
}

func (b *Bot) resolveDirectPeer(ctx context.Context, roomID id.RoomID) (id.UserID, bool, error) {
	if cached, ok := b.cachedRoomPeer(roomID); ok {
		return cached, true, nil
	}

	members, err := b.client.JoinedMembers(ctx, roomID)
	if err != nil {
		return "", false, fmt.Errorf("load joined members: %w", err)
	}
	if len(members.Joined) != 2 {
		return "", false, nil
	}

	for userID := range members.Joined {
		if userID == b.client.UserID {
			continue
		}
		b.rememberRoomPeer(roomID, userID)
		return userID, true, nil
	}
	return "", false, nil
}

func (b *Bot) rememberRoomPeer(roomID id.RoomID, userID id.UserID) {
	b.roomPeersMu.Lock()
	defer b.roomPeersMu.Unlock()

	b.roomPeers[roomID] = userID
}

func (b *Bot) cachedRoomPeer(roomID id.RoomID) (id.UserID, bool) {
	b.roomPeersMu.Lock()
	defer b.roomPeersMu.Unlock()

	userID, ok := b.roomPeers[roomID]
	return userID, ok
}

func (b *Bot) replyWithFailure(ctx context.Context, roomID id.RoomID, txnID string) error {
	if err := b.sendText(ctx, roomID, txnID, "I hit a temporary internal problem while trying to continue the conversation. Please send that again in a moment."); err != nil {
		b.log.Error("failed to send failure message",
			"room_id", roomID.String(),
			"err", err,
		)
		return err
	}
	return nil
}

func (b *Bot) sendText(ctx context.Context, roomID id.RoomID, txnID, body string) error {
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    body,
	}
	_, err := b.client.SendMessageEvent(ctx, roomID, event.EventMessage, content, mautrix.ReqSendEvent{
		TransactionID: txnID,
	})
	if err != nil {
		return fmt.Errorf("send message event: %w", err)
	}
	return nil
}

func (b *Bot) markHandledEvent(eventID, eventKind string) error {
	if eventID == "" {
		return nil
	}
	if err := b.state.MarkHandled(eventID); err != nil {
		b.log.Error("failed to persist handled event",
			"event_id", eventID,
			"event_kind", eventKind,
			"err", err,
		)
		return err
	}
	return nil
}

func requestMetadata(roomID id.RoomID, userID id.UserID, mode, trigger string) map[string]any {
	return map[string]any{
		"matrix": map[string]any{
			"room_id": roomID.String(),
			"user_id": userID.String(),
		},
		"workflow": map[string]any{
			"mode":    mode,
			"trigger": trigger,
		},
	}
}

func conversationMode(record userRecord) string {
	if record.Onboarded() {
		return "general"
	}
	return "onboarding"
}

func (b *Bot) syncWithResume(ctx context.Context) error {
	syncer := b.client.Syncer.(*mautrix.DefaultSyncer)
	snapshot := b.state.Snapshot()
	filterID := snapshot.Sync.FilterID
	nextBatch := snapshot.Sync.NextBatch

	if filterID == "" {
		filter, err := b.client.CreateFilter(ctx, syncer.GetFilterJSON(b.client.UserID))
		if err != nil {
			return fmt.Errorf("create sync filter: %w", err)
		}
		filterID = filter.FilterID
	}

	lastSuccessfulSync := time.Now().Add(-b.client.StreamSyncMinAge - time.Hour)
	isFailing := true

	for {
		streamResp := false
		if b.client.StreamSyncMinAge > 0 && time.Since(lastSuccessfulSync) > b.client.StreamSyncMinAge {
			streamResp = true
		}

		timeout := 30000
		if isFailing || nextBatch == "" {
			timeout = 0
		}

		resp, err := b.client.FullSyncRequest(ctx, mautrix.ReqSync{
			Timeout:        timeout,
			Since:          nextBatch,
			FilterID:       filterID,
			FullState:      false,
			SetPresence:    b.client.SyncPresence,
			StreamResponse: streamResp,
		})
		if err != nil {
			isFailing = true
			if ctx.Err() != nil {
				return ctx.Err()
			}
			waitDuration, syncErr := b.client.Syncer.OnFailedSync(resp, err)
			if syncErr != nil {
				return syncErr
			}
			if waitDuration <= 0 {
				continue
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(waitDuration):
				continue
			}
		}

		isFailing = false
		lastSuccessfulSync = time.Now()
		if nextBatch == "" {
			stripInitialRoomHistory(resp)
		}
		if err := b.client.Syncer.ProcessResponse(ctx, resp, nextBatch); err != nil {
			return fmt.Errorf("process sync response: %w", err)
		}

		nextBatch = resp.NextBatch
		if err := b.state.SaveSyncCursor(state.SyncCursor{
			FilterID:  filterID,
			NextBatch: nextBatch,
		}); err != nil {
			return fmt.Errorf("persist sync cursor: %w", err)
		}
	}
}

func (b *Bot) persistSession() error {
	return b.state.StoreSession(state.Session{
		HomeserverURL: b.config.HomeserverURL,
		UserID:        b.client.UserID.String(),
		AccessToken:   b.client.AccessToken,
		DeviceID:      b.client.DeviceID.String(),
	})
}

func (b *Bot) canRelogin() bool {
	return b.config.Username != "" && b.config.Password != ""
}

func resolveInitialSession(cfg config.Config, snapshot state.FileState) (state.Session, error) {
	if snapshot.Session.HomeserverURL == cfg.HomeserverURL &&
		snapshot.Session.UserID != "" &&
		snapshot.Session.AccessToken != "" &&
		snapshotMatchesConfiguredLogin(cfg, snapshot.Session.UserID) {
		return snapshot.Session, nil
	}

	return state.Session{}, nil
}

func snapshotMatchesConfiguredLogin(cfg config.Config, storedUserID string) bool {
	if cfg.Username == "" {
		return true
	}
	if strings.HasPrefix(cfg.Username, "@") {
		return cfg.Username == storedUserID
	}

	localpart, _, err := id.UserID(storedUserID).Parse()
	if err != nil {
		return false
	}
	return cfg.Username == localpart
}

func stripInitialRoomHistory(resp *mautrix.RespSync) {
	resp.Presence.Events = nil
	resp.AccountData.Events = nil
	for _, room := range resp.Rooms.Join {
		room.Timeline.Events = nil
		room.State.Events = nil
		room.Ephemeral.Events = nil
		room.AccountData.Events = nil
		if room.StateAfter != nil {
			room.StateAfter.Events = nil
		}
	}
}

func stableTransactionID(kind, input string) string {
	sum := sha256.Sum256([]byte(kind + ":" + input))
	return fmt.Sprintf("onboarding_%s_%x", kind, sum[:8])
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
