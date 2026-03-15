package a2a

import (
	"context"
	"fmt"
	"strings"
	"time"

	a2aproto "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2aclient/agentcard"
)

type Request struct {
	Text      string
	TaskID    string
	ContextID string
	Metadata  map[string]any
}

type Response struct {
	TaskID    string
	ContextID string
	State     a2aproto.TaskState
	Reply     string
}

type Client struct {
	client *a2aclient.Client
}

func New(ctx context.Context, baseURL string) (*Client, error) {
	card, err := agentcard.DefaultResolver.Resolve(ctx, baseURL)
	if err != nil {
		return nil, fmt.Errorf("resolve upstream A2A agent card: %w", err)
	}

	client, err := a2aclient.NewFromCard(ctx, card)
	if err != nil {
		return nil, fmt.Errorf("create A2A client: %w", err)
	}

	return &Client{client: client}, nil
}

func (c *Client) Send(ctx context.Context, req Request) (Response, error) {
	blocking := true
	historyLength := 8

	params := &a2aproto.MessageSendParams{
		Config: &a2aproto.MessageSendConfig{
			Blocking:      &blocking,
			HistoryLength: &historyLength,
		},
		Message:  buildMessage(req),
		Metadata: req.Metadata,
	}

	var (
		result a2aproto.SendMessageResult
		err    error
	)
	for attempt := 0; attempt < 5; attempt++ {
		result, err = c.client.SendMessage(ctx, params)
		if err == nil {
			break
		}
		if req.TaskID == "" {
			return Response{}, fmt.Errorf("send upstream A2A message: %w", err)
		}

		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	if err != nil {
		return Response{}, fmt.Errorf("send upstream A2A message: %w", err)
	}

	response, err := normalizeResult(result)
	if err != nil {
		return Response{}, err
	}
	if response.TaskID == "" || response.Reply != "" {
		return response, nil
	}

	// Some agents still answer a blocking send with an initial task state before
	// any user-visible reply is attached. Poll until the task yields a usable
	// response or reaches a state that requires caller intervention.
	return c.waitForTaskUpdate(ctx, response.TaskID)
}

func (c *Client) CancelTask(ctx context.Context, taskID string) error {
	if strings.TrimSpace(taskID) == "" {
		return nil
	}

	if _, err := c.client.CancelTask(ctx, &a2aproto.TaskIDParams{ID: a2aproto.TaskID(taskID)}); err != nil {
		return fmt.Errorf("cancel task %s: %w", taskID, err)
	}
	return nil
}

func (c *Client) waitForTaskUpdate(ctx context.Context, taskID string) (Response, error) {
	historyLength := 8
	deadline := time.Now().Add(10 * time.Second)

	for {
		task, err := c.client.GetTask(ctx, &a2aproto.TaskQueryParams{
			ID:            a2aproto.TaskID(taskID),
			HistoryLength: &historyLength,
		})
		if err != nil {
			return Response{}, fmt.Errorf("get task %s: %w", taskID, err)
		}

		response, err := normalizeResult(task)
		if err != nil {
			return Response{}, err
		}
		if responseReady(response) {
			return response, nil
		}
		if time.Now().After(deadline) {
			return Response{}, fmt.Errorf("task %s did not produce a usable reply before timeout", taskID)
		}

		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func responseReady(response Response) bool {
	if response.Reply != "" {
		return true
	}
	if response.State.Terminal() {
		return true
	}

	switch response.State {
	case a2aproto.TaskStateInputRequired, a2aproto.TaskStateAuthRequired:
		return true
	default:
		return false
	}
}

func buildMessage(req Request) *a2aproto.Message {
	part := a2aproto.TextPart{Text: req.Text}
	if req.TaskID != "" || req.ContextID != "" {
		return a2aproto.NewMessageForTask(
			a2aproto.MessageRoleUser,
			a2aproto.TaskInfo{
				TaskID:    a2aproto.TaskID(req.TaskID),
				ContextID: req.ContextID,
			},
			part,
		)
	}

	return a2aproto.NewMessage(a2aproto.MessageRoleUser, part)
}

func normalizeResult(result a2aproto.SendMessageResult) (Response, error) {
	switch value := result.(type) {
	case *a2aproto.Task:
		resp := Response{
			TaskID:    string(value.ID),
			ContextID: value.ContextID,
			State:     value.Status.State,
		}
		if err := applyMessage(&resp, value.Status.Message); err != nil {
			return Response{}, err
		}
		if resp.Reply == "" {
			applyArtifactFallback(&resp, value.Artifacts)
		}
		return resp, nil
	case *a2aproto.Message:
		resp := Response{State: a2aproto.TaskStateCompleted}
		if err := applyMessage(&resp, value); err != nil {
			return Response{}, err
		}
		return resp, nil
	default:
		return Response{}, fmt.Errorf("unsupported A2A result type %T", result)
	}
}

func applyMessage(resp *Response, msg *a2aproto.Message) error {
	if msg == nil {
		return nil
	}

	var paragraphs []string
	for _, part := range msg.Parts {
		switch value := part.(type) {
		case a2aproto.TextPart:
			text := strings.TrimSpace(value.Text)
			if text != "" {
				paragraphs = append(paragraphs, text)
			}
		case *a2aproto.TextPart:
			text := strings.TrimSpace(value.Text)
			if text != "" {
				paragraphs = append(paragraphs, text)
			}
		}
	}

	resp.Reply = strings.TrimSpace(strings.Join(paragraphs, "\n\n"))
	return nil
}

func applyArtifactFallback(resp *Response, artifacts []*a2aproto.Artifact) {
	for i := len(artifacts) - 1; i >= 0; i-- {
		var paragraphs []string
		for _, part := range artifacts[i].Parts {
			switch value := part.(type) {
			case a2aproto.TextPart:
				text := strings.TrimSpace(value.Text)
				if text != "" {
					paragraphs = append(paragraphs, text)
				}
			case *a2aproto.TextPart:
				text := strings.TrimSpace(value.Text)
				if text != "" {
					paragraphs = append(paragraphs, text)
				}
			}
		}
		if len(paragraphs) == 0 {
			continue
		}
		resp.Reply = strings.Join(paragraphs, "\n\n")
		return
	}
}
