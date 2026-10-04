package rpc_test

import (
	"context"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/harness"
	"github.com/maruina/trygalle/internal/rpc"
)

// TestLiveGetStateAndCommands proves the first protocol proof slice: a live
// get_state/get_commands round-trip against the real pinned pi binary.
func TestLiveGetStateAndCommands(t *testing.T) {
	bin := harness.Pi(t)
	client := harness.Start(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Send(ctx, rpc.GetStateCommand())
	if err != nil {
		t.Fatalf("get_state: %v", err)
	}
	if !resp.Success {
		t.Fatalf("get_state failed: %s", resp.Error)
	}
	var state rpc.State
	if err := resp.Decode(&state); err != nil {
		t.Fatalf("decode get_state data: %v", err)
	}
	if state.SessionID == "" {
		t.Fatal("get_state sessionId is empty")
	}

	resp, err = client.Send(ctx, rpc.GetCommandsCommand())
	if err != nil {
		t.Fatalf("get_commands: %v", err)
	}
	if !resp.Success {
		t.Fatalf("get_commands failed: %s", resp.Error)
	}
	var commands rpc.CommandsData
	if err := resp.Decode(&commands); err != nil {
		t.Fatalf("decode get_commands data: %v", err)
	}
	if len(commands.Commands) == 0 {
		t.Fatal("get_commands returned an empty command list")
	}
}
