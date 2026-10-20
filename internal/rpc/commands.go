package rpc

import (
	"context"
	"fmt"
)

// Prompt sends a prompt command with streamingBehavior "steer" and returns the
// disposition Pi reports. prompt{steer} is the only submission form Trygalle
// routes with: Pi decides between started, queued, and handled (see the
// pi-runtime-and-rpc design, "Message routing").
func (c *Client) Prompt(ctx context.Context, message string) (Disposition, error) {
	return c.sendDisposition(ctx, PromptCommand(message, "steer"))
}

// Steer sends a bare steering command and returns the disposition Pi reports.
// It is the compaction-window fallback for a failed prompt.
func (c *Client) Steer(ctx context.Context, message string) (Disposition, error) {
	return c.sendDisposition(ctx, SteerCommand(message))
}

func (c *Client) sendDisposition(ctx context.Context, cmd Command) (Disposition, error) {
	var data PromptData
	if err := c.sendDecode(ctx, cmd, &data); err != nil {
		return "", err
	}
	return data.Disposition, nil
}

// GetState returns the session state Pi reports.
func (c *Client) GetState(ctx context.Context) (State, error) {
	var s State
	if err := c.sendDecode(ctx, GetStateCommand(), &s); err != nil {
		return State{}, err
	}
	return s, nil
}

// GetCommands returns the commands Pi reports.
func (c *Client) GetCommands(ctx context.Context) ([]CommandInfo, error) {
	var data CommandsData
	if err := c.sendDecode(ctx, GetCommandsCommand(), &data); err != nil {
		return nil, err
	}
	return data.Commands, nil
}

// Abort aborts the current operation.
func (c *Client) Abort(ctx context.Context) error {
	_, err := c.sendOK(ctx, AbortCommand())
	return err
}

// ClearQueue removes queued steering and follow-up messages and returns their
// text.
func (c *Client) ClearQueue(ctx context.Context) (QueueContents, error) {
	var q QueueContents
	if err := c.sendDecode(ctx, ClearQueueCommand(), &q); err != nil {
		return QueueContents{}, err
	}
	return q, nil
}

// NewSession starts a fresh session. A session_before_switch extension handler
// can cancel the switch; cancelled reports that outcome.
func (c *Client) NewSession(ctx context.Context) (cancelled bool, err error) {
	var data NewSessionData
	if err := c.sendDecode(ctx, NewSessionCommand(), &data); err != nil {
		return false, err
	}
	return data.Cancelled, nil
}

// GetLastAssistantText returns the last assistant text in the session. A nil
// pointer is JSON null: no assistant text exists.
func (c *Client) GetLastAssistantText(ctx context.Context) (*string, error) {
	var data LastAssistantTextData
	if err := c.sendDecode(ctx, GetLastAssistantTextCommand(), &data); err != nil {
		return nil, err
	}
	return data.Text, nil
}

// sendDecode sends one command, requires success, and decodes the response
// data into v. An error response carries Pi's error text.
func (c *Client) sendDecode(ctx context.Context, cmd Command, v any) error {
	resp, err := c.sendOK(ctx, cmd)
	if err != nil {
		return err
	}
	return resp.Decode(v)
}

func (c *Client) sendOK(ctx context.Context, cmd Command) (Response, error) {
	resp, err := c.Send(ctx, cmd)
	if err != nil {
		return Response{}, err
	}
	if !resp.Success {
		return Response{}, fmt.Errorf("pi %s command failed: %s", cmd.Type, resp.Error)
	}
	return resp, nil
}
