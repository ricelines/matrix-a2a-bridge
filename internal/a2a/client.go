package a2a

import (
	"context"
	"fmt"
	"strings"

	a2aproto "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2aclient/agentcard"
)

type Notification struct {
	Body     string
	Metadata map[string]any
}

type DeliveryOptions struct {
	MessageID       string
	ContextID       string
	ReferenceTaskID string
}

type Client struct {
	client *a2aclient.Client
}

func New(ctx context.Context, baseURL string) (*Client, error) {
	card, err := agentcard.DefaultResolver.Resolve(ctx, baseURL)
	if err != nil {
		return nil, fmt.Errorf("resolve upstream A2A agent card: %w", err)
	}

	client, err := a2aclient.NewFromCard(
		ctx,
		card,
		a2aclient.WithConfig(a2aclient.Config{Polling: true}),
	)
	if err != nil {
		return nil, fmt.Errorf("create A2A client: %w", err)
	}

	return &Client{client: client}, nil
}

func (c *Client) Deliver(ctx context.Context, notification Notification, options DeliveryOptions) (*a2aproto.Task, error) {
	body := strings.TrimSpace(notification.Body)
	if body == "" {
		return nil, fmt.Errorf("event notification body must not be empty")
	}

	message := a2aproto.NewMessage(
		a2aproto.MessageRoleUser,
		a2aproto.TextPart{Text: body},
	)
	if options.MessageID != "" {
		message.ID = options.MessageID
	}
	if options.ContextID != "" {
		message.ContextID = options.ContextID
	}
	if options.ReferenceTaskID != "" {
		message.ReferenceTasks = []a2aproto.TaskID{a2aproto.TaskID(options.ReferenceTaskID)}
	}

	blocking := false
	result, err := c.client.SendMessage(ctx, &a2aproto.MessageSendParams{
		Config: &a2aproto.MessageSendConfig{
			Blocking: &blocking,
		},
		Message:  message,
		Metadata: notification.Metadata,
	})
	if err != nil {
		return nil, fmt.Errorf("send upstream A2A notification: %w", err)
	}

	task, ok := result.(*a2aproto.Task)
	if !ok {
		return nil, fmt.Errorf("send upstream A2A notification: expected task result, got %T", result)
	}
	return task, nil
}

func (c *Client) GetTask(ctx context.Context, taskID string) (*a2aproto.Task, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, fmt.Errorf("task id must not be empty")
	}

	task, err := c.client.GetTask(ctx, &a2aproto.TaskQueryParams{ID: a2aproto.TaskID(taskID)})
	if err != nil {
		return nil, fmt.Errorf("get upstream A2A task %s: %w", taskID, err)
	}
	return task, nil
}
