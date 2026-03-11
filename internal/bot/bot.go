package bot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"onboarding/internal/config"
	"onboarding/internal/state"
)

type Bot struct {
	client *mautrix.Client
	config config.Config
	log    *slog.Logger
	state  *state.Store
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
		client: client,
		config: cfg,
		log:    logger,
		state:  stateStore,
	}
	bot.registerHandlers()
	return bot, nil
}

func (b *Bot) Run(ctx context.Context) error {
	if err := b.authenticate(ctx); err != nil {
		return err
	}

	b.log.Info("starting matrix bot",
		"user_id", b.client.UserID.String(),
		"homeserver", b.client.HomeserverURL.String(),
		"command_prefix", b.config.CommandPrefix,
		"state_path", b.config.StatePath,
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
		InitialDeviceDisplayName: "matrix-bot",
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
	if !b.config.AutoJoinInvites {
		return
	}
	if evt.GetStateKey() != b.client.UserID.String() {
		return
	}
	if evt.Content.AsMember().Membership != event.MembershipInvite {
		return
	}

	if _, err := b.client.JoinRoomByID(ctx, evt.RoomID); err != nil {
		b.log.Error("failed to join invited room",
			"room_id", evt.RoomID.String(),
			"sender", evt.Sender.String(),
			"err", err,
		)
		return
	}

	b.log.Info("joined invited room",
		"room_id", evt.RoomID.String(),
		"sender", evt.Sender.String(),
	)
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
	if !strings.HasPrefix(body, b.config.CommandPrefix) {
		return
	}

	reply := b.handleCommand(strings.TrimSpace(strings.TrimPrefix(body, b.config.CommandPrefix)))
	if reply == "" {
		return
	}

	content := &event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    reply,
	}
	if _, err := b.client.SendMessageEvent(ctx, evt.RoomID, event.EventMessage, content, mautrix.ReqSendEvent{
		TransactionID: stableTransactionID("command-reply", evt.ID.String()),
	}); err != nil {
		b.log.Error("failed to send reply",
			"room_id", evt.RoomID.String(),
			"event_id", evt.ID.String(),
			"err", err,
		)
		return
	}
	if evt.ID != "" {
		if err := b.state.MarkHandled(evt.ID.String()); err != nil {
			b.log.Error("failed to persist handled event",
				"room_id", evt.RoomID.String(),
				"event_id", evt.ID.String(),
				"err", err,
			)
		}
	}

	b.log.Info("replied to command",
		"room_id", evt.RoomID.String(),
		"sender", evt.Sender.String(),
		"command", body,
	)
}

func (b *Bot) handleCommand(commandLine string) string {
	if commandLine == "" {
		return fmt.Sprintf("Try %shelp", b.config.CommandPrefix)
	}

	fields := strings.Fields(commandLine)
	switch strings.ToLower(fields[0]) {
	case "help":
		return fmt.Sprintf("Commands: %sping, %secho <text>, %shelp", b.config.CommandPrefix, b.config.CommandPrefix, b.config.CommandPrefix)
	case "ping":
		return "pong"
	case "echo":
		payload := strings.TrimSpace(strings.TrimPrefix(commandLine, fields[0]))
		if payload == "" {
			return fmt.Sprintf("Usage: %secho <text>", b.config.CommandPrefix)
		}
		return payload
	default:
		return fmt.Sprintf("Unknown command %q. Try %shelp", fields[0], b.config.CommandPrefix)
	}
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
	if cfg.UsingAccessToken() {
		userID := id.UserID(cfg.UserID)
		if _, _, err := userID.ParseAndValidateRelaxed(); err != nil {
			return state.Session{}, fmt.Errorf("parse %s: %w", "MATRIX_USER_ID", err)
		}

		session := state.Session{
			HomeserverURL: cfg.HomeserverURL,
			UserID:        cfg.UserID,
			AccessToken:   cfg.AccessToken,
		}
		if snapshot.Session.HomeserverURL == cfg.HomeserverURL && snapshot.Session.UserID == cfg.UserID {
			session.DeviceID = snapshot.Session.DeviceID
		}
		return session, nil
	}

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
