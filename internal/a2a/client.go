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

func (c *Client) Deliver(ctx context.Context, notification Notification) error {
	body := strings.TrimSpace(notification.Body)
	if body == "" {
		return fmt.Errorf("event notification body must not be empty")
	}

	blocking := false
	_, err := c.client.SendMessage(ctx, &a2aproto.MessageSendParams{
		Config: &a2aproto.MessageSendConfig{
			Blocking: &blocking,
		},
		Message: a2aproto.NewMessage(
			a2aproto.MessageRoleUser,
			a2aproto.TextPart{Text: body},
		),
		Metadata: notification.Metadata,
	})
	if err != nil {
		return fmt.Errorf("send upstream A2A notification: %w", err)
	}
	return nil
}
