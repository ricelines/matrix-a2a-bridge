package bridge

import (
	"context"
	"encoding/json"
	"fmt"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"matrix-a2a-bridge/internal/a2a"
	"matrix-a2a-bridge/internal/state"
)

type roomUpdateNotificationEnvelope struct {
	Kind          string              `json:"kind"`
	BridgeUserID  string              `json:"bridge_user_id"`
	HomeserverURL string              `json:"homeserver_url"`
	RoomID        string              `json:"room_id"`
	Updates       []roomUpdateSegment `json:"updates"`
}

type roomUpdateSegment struct {
	RoomSection string         `json:"room_section"`
	State       []*event.Event `json:"state,omitempty"`
	Timeline    []*event.Event `json:"timeline,omitempty"`
}

type roomUpdateBatch struct {
	RoomID  string
	Updates []roomUpdateSegment
}

func (b *Bridge) handleSyncResponse(ctx context.Context, resp *mautrix.RespSync, _ string) bool {
	for _, batch := range b.collectRoomUpdateBatches(resp) {
		b.enqueueRoomUpdate(ctx, batch)
	}
	return false
}

func (b *Bridge) collectRoomUpdateBatches(resp *mautrix.RespSync) []roomUpdateBatch {
	if resp == nil {
		return nil
	}

	var batches []roomUpdateBatch
	for roomID, roomData := range resp.Rooms.Join {
		stateEvents := roomData.State.Events
		if roomData.StateAfter != nil {
			stateEvents = roomData.StateAfter.Events
		}
		if batch, ok := b.buildRoomUpdateBatch(roomID, "join", stateEvents, roomData.Timeline.Events); ok {
			batches = append(batches, batch)
		}
	}
	for roomID, roomData := range resp.Rooms.Invite {
		if batch, ok := b.buildRoomUpdateBatch(roomID, "invite", roomData.State.Events, nil); ok {
			batches = append(batches, batch)
		}
	}
	for roomID, roomData := range resp.Rooms.Leave {
		if batch, ok := b.buildRoomUpdateBatch(roomID, "leave", roomData.State.Events, roomData.Timeline.Events); ok {
			batches = append(batches, batch)
		}
	}
	return batches
}

func (b *Bridge) buildRoomUpdateBatch(roomID id.RoomID, roomSection string, stateEvents, timelineEvents []*event.Event) (roomUpdateBatch, bool) {
	stateSegment := b.filterRoomEvents(roomID, roomSection, "state", stateEvents)
	timelineSegment := b.filterRoomEvents(roomID, roomSection, "timeline", timelineEvents)
	if len(stateSegment) == 0 && len(timelineSegment) == 0 {
		return roomUpdateBatch{}, false
	}

	return roomUpdateBatch{
		RoomID: roomID.String(),
		Updates: []roomUpdateSegment{{
			RoomSection: roomSection,
			State:       stateSegment,
			Timeline:    timelineSegment,
		}},
	}, true
}

func (b *Bridge) filterRoomEvents(roomID id.RoomID, roomSection, eventSection string, events []*event.Event) []*event.Event {
	source, ok := roomEventSource(roomSection, eventSection)
	if !ok {
		return nil
	}

	filtered := make([]*event.Event, 0, len(events))
	for _, evt := range events {
		if !shouldForwardEventSource(b.client.UserID, evt, source) {
			continue
		}
		if evt != nil && evt.ID != "" && b.state.IsHandled(evt.ID.String()) {
			continue
		}

		cloned := cloneEvent(evt)
		cloned.RoomID = roomID
		cloned.Mautrix.EventSource = source
		filtered = append(filtered, cloned)
	}
	return filtered
}

func buildRoomUpdateNotification(self id.UserID, homeserverURL string, batch roomUpdateBatch) (a2a.Notification, error) {
	if batch.RoomID == "" {
		return a2a.Notification{}, fmt.Errorf("room update batch room id must not be empty")
	}
	if len(batch.Updates) == 0 {
		return a2a.Notification{}, fmt.Errorf("room update batch must include at least one update")
	}

	envelope := roomUpdateNotificationEnvelope{
		Kind:          "matrix_room_update",
		BridgeUserID:  self.String(),
		HomeserverURL: homeserverURL,
		RoomID:        batch.RoomID,
		Updates:       batch.Updates,
	}

	body, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return a2a.Notification{}, fmt.Errorf("marshal room update notification: %w", err)
	}

	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return a2a.Notification{}, fmt.Errorf("decode room update notification metadata: %w", err)
	}

	return a2a.Notification{
		Body: string(body),
		Metadata: map[string]any{
			"matrix_room_update": metadata,
		},
	}, nil
}

func roomEventSource(roomSection, eventSection string) (event.Source, bool) {
	var source event.Source
	switch roomSection {
	case "join":
		source = event.SourceJoin
	case "invite":
		source = event.SourceInvite
	case "leave":
		source = event.SourceLeave
	default:
		return 0, false
	}

	switch eventSection {
	case "timeline":
		source |= event.SourceTimeline
	case "state":
		source |= event.SourceState
	default:
		return 0, false
	}

	return source, true
}

func shouldForwardEvent(self id.UserID, evt *event.Event) bool {
	if evt == nil {
		return false
	}
	return shouldForwardEventSource(self, evt, evt.Mautrix.EventSource)
}

func shouldForwardEventSource(self id.UserID, evt *event.Event, source event.Source) bool {
	if evt == nil {
		return false
	}
	if evt.Sender != "" && evt.Sender == self {
		return false
	}
	_, _, ok := eventSourceSections(source)
	return ok
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

func cloneEvent(evt *event.Event) *event.Event {
	if evt == nil {
		return nil
	}
	cloned := *evt
	return &cloned
}

func mergeRoomUpdateBatches(current, next roomUpdateBatch) roomUpdateBatch {
	switch {
	case current.RoomID == "":
		return next
	case next.RoomID == "":
		return current
	case current.RoomID != next.RoomID:
		panic(fmt.Sprintf("attempted to merge room batches for %s and %s", current.RoomID, next.RoomID))
	default:
		current.Updates = append(current.Updates, next.Updates...)
		return current
	}
}

func (b roomUpdateBatch) messageID() string {
	last := b.lastEvent()
	if last != nil && last.ID != "" {
		return last.ID.String()
	}
	return ""
}

func (b roomUpdateBatch) lastEvent() *event.Event {
	for updateIdx := len(b.Updates) - 1; updateIdx >= 0; updateIdx-- {
		update := b.Updates[updateIdx]
		for idx := len(update.Timeline) - 1; idx >= 0; idx-- {
			if update.Timeline[idx] != nil {
				return update.Timeline[idx]
			}
		}
		for idx := len(update.State) - 1; idx >= 0; idx-- {
			if update.State[idx] != nil {
				return update.State[idx]
			}
		}
	}
	return nil
}

func (b roomUpdateBatch) handledEvents() []state.EventSummary {
	seen := make(map[string]struct{})
	events := make([]state.EventSummary, 0)
	for _, update := range b.Updates {
		events = appendHandledEvents(events, seen, update.State)
		events = appendHandledEvents(events, seen, update.Timeline)
	}
	return events
}

func appendHandledEvents(dst []state.EventSummary, seen map[string]struct{}, events []*event.Event) []state.EventSummary {
	for _, evt := range events {
		if evt == nil || evt.ID == "" {
			continue
		}
		eventID := evt.ID.String()
		if _, ok := seen[eventID]; ok {
			continue
		}
		seen[eventID] = struct{}{}
		dst = append(dst, state.EventSummary{
			ID:   eventID,
			Type: evt.Type.String(),
		})
	}
	return dst
}
