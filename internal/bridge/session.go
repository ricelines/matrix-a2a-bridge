package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	a2aproto "github.com/a2aproject/a2a-go/a2a"

	bridgea2a "matrix-a2a-bridge/internal/a2a"
	"matrix-a2a-bridge/internal/state"
)

const (
	taskTerminalPollInterval = 100 * time.Millisecond
	taskTerminalWaitTimeout  = 30 * time.Second
	roomQueueRetryDelay      = 1 * time.Second
)

type roomQueue struct {
	mu         sync.Mutex
	pending    roomUpdateBatch
	hasPending bool
	running    bool
}

func (b *Bridge) enqueueRoomUpdate(ctx context.Context, batch roomUpdateBatch) {
	if batch.RoomID == "" || len(batch.Updates) == 0 {
		return
	}

	queue := b.roomQueue(batch.RoomID)
	queue.mu.Lock()
	queue.pending = mergeRoomUpdateBatches(queue.pending, batch)
	queue.hasPending = true
	if queue.running {
		queue.mu.Unlock()
		return
	}
	queue.running = true
	queue.mu.Unlock()

	go b.runRoomQueue(b.deliveryContext(ctx), queue)
}

func (b *Bridge) roomQueue(roomID string) *roomQueue {
	b.roomsMu.Lock()
	defer b.roomsMu.Unlock()

	if b.rooms == nil {
		b.rooms = make(map[string]*roomQueue)
	}

	queue := b.rooms[roomID]
	if queue == nil {
		queue = &roomQueue{}
		b.rooms[roomID] = queue
	}
	return queue
}

func (b *Bridge) deliveryContext(fallback context.Context) context.Context {
	if b.runCtx != nil {
		return b.runCtx
	}
	if fallback != nil {
		return fallback
	}
	return context.Background()
}

func (b *Bridge) runRoomQueue(ctx context.Context, queue *roomQueue) {
	for {
		queue.mu.Lock()
		if !queue.hasPending {
			queue.running = false
			queue.mu.Unlock()
			return
		}
		batch := queue.pending
		queue.pending = roomUpdateBatch{}
		queue.hasPending = false
		queue.mu.Unlock()

		if err := b.deliverRoomUpdate(ctx, batch); err != nil {
			b.log.Error("failed to deliver matrix room update to upstream A2A",
				"room_id", batch.RoomID,
				"err", err,
			)
			if ctx.Err() != nil {
				return
			}

			queue.mu.Lock()
			queue.pending = mergeRoomUpdateBatches(batch, queue.pending)
			queue.hasPending = true
			queue.mu.Unlock()

			select {
			case <-ctx.Done():
				return
			case <-time.After(roomQueueRetryDelay):
			}
		}
	}
}

func (b *Bridge) deliverRoomUpdate(ctx context.Context, batch roomUpdateBatch) error {
	notification, err := buildRoomUpdateNotification(b.client.UserID, b.config.HomeserverURL, batch)
	if err != nil {
		return err
	}

	session := b.loadRoomSession(batch.RoomID)
	if err := b.waitForRoomTaskTerminal(ctx, &session); err != nil {
		return err
	}

	task, updatedSession, err := b.deliverWithRecovery(ctx, batch, notification, session)
	if err != nil {
		return err
	}

	b.log.Info("delivered matrix room update to upstream A2A",
		"room_id", batch.RoomID,
		"context_id", updatedSession.ContextID,
		"task_id", task.ID,
		"task_state", task.Status.State,
		"update_count", len(batch.Updates),
	)

	return b.state.RecordRoomDeliveryBatch(updatedSession, batch.handledEvents())
}

func (b *Bridge) loadRoomSession(roomID string) state.RoomSession {
	if session, ok := b.state.RoomSession(roomID); ok {
		if session.ContextID == "" {
			session.ContextID = a2aproto.NewContextID()
		}
		return session
	}
	return state.RoomSession{
		RoomID:    roomID,
		ContextID: a2aproto.NewContextID(),
	}
}

func (b *Bridge) waitForRoomTaskTerminal(ctx context.Context, session *state.RoomSession) error {
	if session.LatestTaskID == "" {
		return nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, taskTerminalWaitTimeout)
	defer cancel()

	for {
		task, err := b.upstream.GetTask(waitCtx, session.LatestTaskID)
		if err != nil {
			if errors.Is(err, a2aproto.ErrTaskNotFound) {
				session.LatestTaskID = ""
				return nil
			}
			return err
		}
		if task.Status.State.Terminal() {
			return nil
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for upstream task %s in room %s to become terminal: %w", session.LatestTaskID, session.RoomID, waitCtx.Err())
		case <-time.After(taskTerminalPollInterval):
		}
	}
}

func (b *Bridge) deliverWithRecovery(
	ctx context.Context,
	batch roomUpdateBatch,
	notification bridgea2a.Notification,
	session state.RoomSession,
) (*a2aproto.Task, state.RoomSession, error) {
	task, updatedSession, err := b.deliverAttempt(ctx, batch, notification, session)
	if err != nil {
		return nil, session, err
	}
	if !isContinuationFailure(task) {
		return task, updatedSession, nil
	}

	if updatedSession.LatestTaskID != "" {
		b.log.Warn("room continuation task failed; retrying without reference task",
			"room_id", updatedSession.RoomID,
			"context_id", updatedSession.ContextID,
			"failed_task_id", task.ID,
			"failure", taskFailureText(task),
		)
		withoutReference := updatedSession
		withoutReference.LatestTaskID = ""
		task, retriedSession, err := b.deliverAttempt(ctx, batch, notification, withoutReference)
		if err != nil {
			return nil, session, err
		}
		if !isContinuationFailure(task) {
			return task, retriedSession, nil
		}
		updatedSession = retriedSession
	}

	b.log.Warn("room context failed; retrying with fresh context",
		"room_id", updatedSession.RoomID,
		"old_context_id", updatedSession.ContextID,
		"failure", taskFailureText(task),
	)
	freshContext := updatedSession
	freshContext.ContextID = a2aproto.NewContextID()
	freshContext.LatestTaskID = ""
	task, freshSession, err := b.deliverAttempt(ctx, batch, notification, freshContext)
	if err != nil {
		return nil, session, err
	}
	if isContinuationFailure(task) {
		return nil, session, fmt.Errorf("upstream rejected fresh room context for room %s: %s", session.RoomID, taskFailureText(task))
	}
	return task, freshSession, nil
}

func (b *Bridge) deliverAttempt(
	ctx context.Context,
	batch roomUpdateBatch,
	notification bridgea2a.Notification,
	session state.RoomSession,
) (*a2aproto.Task, state.RoomSession, error) {
	task, err := b.upstream.Deliver(ctx, notification, bridgea2a.DeliveryOptions{
		MessageID:       messageIDForBatch(batch),
		ContextID:       session.ContextID,
		ReferenceTaskID: session.LatestTaskID,
	})
	if err != nil {
		return nil, session, err
	}
	if task == nil {
		return nil, session, fmt.Errorf("upstream did not return a task for room %s", session.RoomID)
	}
	if task.ID == "" {
		return nil, session, fmt.Errorf("upstream returned task without id for room %s", session.RoomID)
	}
	if task.ContextID == "" {
		return nil, session, fmt.Errorf("upstream returned task %s without context id", task.ID)
	}
	if session.ContextID != "" && task.ContextID != session.ContextID {
		return nil, session, fmt.Errorf("upstream changed room %s context from %s to %s", session.RoomID, session.ContextID, task.ContextID)
	}

	session.ContextID = task.ContextID
	session.LatestTaskID = string(task.ID)
	return task, session, nil
}

func messageIDForBatch(batch roomUpdateBatch) string {
	if messageID := strings.TrimSpace(batch.messageID()); messageID != "" {
		return messageID
	}
	return a2aproto.NewMessageID()
}

func isContinuationFailure(task *a2aproto.Task) bool {
	if task == nil || task.Status.State != a2aproto.TaskStateFailed {
		return false
	}

	text := strings.ToLower(taskFailureText(task))
	if text == "" {
		return false
	}

	for _, needle := range []string{
		"reference task",
		"referenced tasks",
		"referencetaskids",
		"multiple task branches",
		"message contextid different from task contextid",
		"wait for it to finish",
		"still active",
	} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func taskFailureText(task *a2aproto.Task) string {
	if task == nil || task.Status.Message == nil {
		return ""
	}
	for _, part := range task.Status.Message.Parts {
		switch value := part.(type) {
		case a2aproto.TextPart:
			return strings.TrimSpace(value.Text)
		case *a2aproto.TextPart:
			return strings.TrimSpace(value.Text)
		}
	}
	return ""
}
