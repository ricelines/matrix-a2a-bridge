package agent

import (
	"context"
	"encoding/json"
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

type Command struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type Response struct {
	TaskID             string
	ContextID          string
	State              a2aproto.TaskState
	Reply              string
	Commands           []Command
	CompleteOnboarding bool
	CloseSession       bool
}

type controlPayload struct {
	Commands           []Command `json:"commands,omitempty"`
	CompleteOnboarding bool      `json:"complete_onboarding,omitempty"`
	CloseSession       bool      `json:"close_session,omitempty"`
}

type Client struct {
	client *a2aclient.Client
}

func New(ctx context.Context, baseURL string) (*Client, error) {
	card, err := agentcard.DefaultResolver.Resolve(ctx, baseURL)
	if err != nil {
		return nil, fmt.Errorf("resolve agent card: %w", err)
	}

	client, err := a2aclient.NewFromCard(ctx, card)
	if err != nil {
		return nil, fmt.Errorf("create A2A client: %w", err)
	}

	return &Client{client: client}, nil
}

func (c *Client) Send(ctx context.Context, req Request) (Response, error) {
	blocking := false
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
			return Response{}, fmt.Errorf("send message: %w", err)
		}

		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	if err != nil {
		return Response{}, fmt.Errorf("send message: %w", err)
	}

	response, err := normalizeResult(result)
	if err != nil {
		return Response{}, err
	}
	if response.TaskID == "" || response.Reply != "" {
		return response, nil
	}

	// Non-blocking sends can interrupt on the initial submitted task. Re-read the
	// task briefly so the caller receives the actual agent reply for this turn.
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

	for i := 0; i < 20; i++ {
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
		if response.Reply != "" || response.State != a2aproto.TaskStateSubmitted {
			return response, nil
		}

		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}

	task, err := c.client.GetTask(ctx, &a2aproto.TaskQueryParams{ID: a2aproto.TaskID(taskID)})
	if err != nil {
		return Response{}, fmt.Errorf("get task %s after retries: %w", taskID, err)
	}
	return normalizeResult(task)
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
		case a2aproto.DataPart:
			if err := applyControl(resp, value.Data); err != nil {
				return err
			}
		case *a2aproto.DataPart:
			if err := applyControl(resp, value.Data); err != nil {
				return err
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

func applyControl(resp *Response, data map[string]any) error {
	if len(data) == 0 {
		return nil
	}

	payloadBytes, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal control payload: %w", err)
	}

	var payload controlPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return fmt.Errorf("decode control payload: %w", err)
	}

	resp.Commands = append(resp.Commands, payload.Commands...)
	resp.CompleteOnboarding = resp.CompleteOnboarding || payload.CompleteOnboarding
	resp.CloseSession = resp.CloseSession || payload.CloseSession
	return nil
}
