package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"matrix-a2a-bridge/internal/a2a"
	"matrix-a2a-bridge/internal/config"
	"matrix-a2a-bridge/internal/state"
)

type Bridge struct {
	client   *mautrix.Client
	config   config.Config
	log      *slog.Logger
	state    *state.Store
	upstream *a2a.Client

	roomLocksMu sync.Mutex
	roomLocks   map[string]*sync.Mutex
}

func New(cfg config.Config, logger *slog.Logger) (*Bridge, error) {
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

	matrixBridge := &Bridge{
		client:    client,
		config:    cfg,
		log:       logger,
		state:     stateStore,
		roomLocks: make(map[string]*sync.Mutex),
	}
	matrixBridge.registerHandlers()
	return matrixBridge, nil
}

func (b *Bridge) Run(ctx context.Context) error {
	if err := b.authenticate(ctx); err != nil {
		return err
	}

	upstreamClient, err := a2a.New(ctx, b.config.UpstreamA2AURL)
	if err != nil {
		return fmt.Errorf("connect to upstream A2A endpoint: %w", err)
	}
	b.upstream = upstreamClient

	b.log.Info("starting Matrix A2A bridge runtime",
		"user_id", b.client.UserID.String(),
		"homeserver", b.client.HomeserverURL.String(),
		"state_path", b.config.StatePath,
		"upstream_a2a_url", b.config.UpstreamA2AURL,
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

func (b *Bridge) authenticate(ctx context.Context) error {
	if b.client.AccessToken != "" && b.client.UserID != "" {
		if err := b.persistSession(); err != nil {
			return err
		}
		return nil
	}

	return b.loginWithPassword(ctx)
}

func (b *Bridge) loginWithPassword(ctx context.Context) error {
	loginReq := &mautrix.ReqLogin{
		Type: mautrix.AuthTypePassword,
		Identifier: mautrix.UserIdentifier{
			Type: mautrix.IdentifierTypeUser,
			User: usernameForLogin(b.config.Username),
		},
		Password:                 b.config.Password,
		InitialDeviceDisplayName: "matrix-a2a-bridge",
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

func (b *Bridge) registerHandlers() {
	syncer := b.client.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEvent(b.handleEvent)
}

func (b *Bridge) syncWithResume(ctx context.Context) error {
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
			stripInitialBacklog(resp)
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

func (b *Bridge) persistSession() error {
	return b.state.StoreSession(state.Session{
		HomeserverURL: b.config.HomeserverURL,
		UserID:        b.client.UserID.String(),
		AccessToken:   b.client.AccessToken,
		DeviceID:      b.client.DeviceID.String(),
	})
}

func (b *Bridge) canRelogin() bool {
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

func stripInitialBacklog(resp *mautrix.RespSync) {
	resp.ToDevice.Events = nil
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
	for _, room := range resp.Rooms.Leave {
		room.Timeline.Events = nil
		room.State.Events = nil
	}
}
