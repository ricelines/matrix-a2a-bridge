package bridge

import (
	"context"
	"encoding/json"
	"fmt"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"matrix-a2a-bridge/internal/a2a"
)

type eventNotificationEnvelope struct {
	Kind          string                  `json:"kind"`
	BridgeUserID  string                  `json:"bridge_user_id"`
	HomeserverURL string                  `json:"homeserver_url"`
	Source        eventNotificationSource `json:"source"`
	Event         *event.Event            `json:"event"`
}

type eventNotificationSource struct {
	Description  string `json:"description"`
	RoomSection  string `json:"room_section"`
	EventSection string `json:"event_section"`
}

func (b *Bridge) handleEvent(ctx context.Context, evt *event.Event) {
	if !shouldForwardEvent(b.client.UserID, evt) {
		return
	}
	if evt.ID != "" && b.state.IsHandled(evt.ID.String()) {
		return
	}

	notification, err := buildEventNotification(b.client.UserID, b.config.HomeserverURL, evt)
	if err != nil {
		b.log.Error("failed to encode matrix event notification",
			"room_id", evt.RoomID.String(),
			"event_id", evt.ID.String(),
			"event_type", evt.Type.String(),
			"source", evt.Mautrix.EventSource.String(),
			"err", err,
		)
		return
	}

	if err := b.deliverRoomEvent(ctx, evt, notification); err != nil {
		b.log.Error("failed to deliver matrix event to upstream A2A",
			"room_id", evt.RoomID.String(),
			"event_id", evt.ID.String(),
			"event_type", evt.Type.String(),
			"source", evt.Mautrix.EventSource.String(),
			"err", err,
		)
		return
	}

}

func shouldForwardEvent(self id.UserID, evt *event.Event) bool {
	if evt == nil {
		return false
	}
	if evt.Sender != "" && evt.Sender == self {
		return false
	}
	_, _, ok := eventSourceSections(evt.Mautrix.EventSource)
	return ok
}

func buildEventNotification(self id.UserID, homeserverURL string, evt *event.Event) (a2a.Notification, error) {
	roomSection, eventSection, ok := eventSourceSections(evt.Mautrix.EventSource)
	if !ok {
		return a2a.Notification{}, fmt.Errorf("unsupported event source %q", evt.Mautrix.EventSource.String())
	}

	envelope := eventNotificationEnvelope{
		Kind:          "matrix_event",
		BridgeUserID:  self.String(),
		HomeserverURL: homeserverURL,
		Source: eventNotificationSource{
			Description:  evt.Mautrix.EventSource.String(),
			RoomSection:  roomSection,
			EventSection: eventSection,
		},
		Event: evt,
	}

	body, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return a2a.Notification{}, fmt.Errorf("marshal event notification: %w", err)
	}

	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return a2a.Notification{}, fmt.Errorf("decode event notification metadata: %w", err)
	}

	return a2a.Notification{
		Body: string(body),
		Metadata: map[string]any{
			"matrix_event": metadata,
		},
	}, nil
}

func eventSourceSections(source event.Source) (roomSection, eventSection string, ok bool) {
	switch {
	case source&event.SourceJoin != 0:
		roomSection = "join"
	case source&event.SourceInvite != 0:
		roomSection = "invite"
	case source&event.SourceLeave != 0:
		roomSection = "leave"
	default:
		return "", "", false
	}

	switch {
	case source&event.SourceTimeline != 0:
		eventSection = "timeline"
	case source&event.SourceState != 0:
		eventSection = "state"
	default:
		return "", "", false
	}

	return roomSection, eventSection, true
}
