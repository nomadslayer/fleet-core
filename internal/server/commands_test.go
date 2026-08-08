package server

import (
	"fmt"
	"testing"

	"fleetcore/internal/api"
)

// Commands live in desired state so their results stay visible, but that
// state is resent to the agent on every reconcile. Without a cap a
// long-lived machine accumulates an unbounded reconcile payload, so the
// trim is load-bearing rather than cosmetic.
func TestPruneCommandsKeepsNewestAndSpareseNonCommands(t *testing.T) {
	var specs []api.ModuleSpec
	// A real module that must survive pruning untouched.
	specs = append(specs, api.ModuleSpec{Name: "dcgm", Version: "1.0.0"})
	// defaultCommandHistory+5 commands, oldest first.
	total := defaultCommandHistory + 5
	for i := 0; i < total; i++ {
		specs = append(specs, api.ModuleSpec{Name: fmt.Sprintf("cmd-%d-aaaaaa", 1000+i), Version: "1"})
	}

	kept, dropped := pruneCommands(specs, defaultCommandHistory)

	if len(dropped) != 5 {
		t.Fatalf("dropped %d, want 5: %v", len(dropped), dropped)
	}
	var keptCmds, keptOther int
	for _, sp := range kept {
		if isCommand(sp.Name) {
			keptCmds++
		} else {
			keptOther++
		}
	}
	if keptCmds != defaultCommandHistory {
		t.Errorf("kept %d commands, want %d", keptCmds, defaultCommandHistory)
	}
	if keptOther != 1 {
		t.Errorf("non-command modules must never be pruned; kept %d of 1", keptOther)
	}
	// The five oldest go, by the timestamp in the name — not by slice order.
	for i := 0; i < 5; i++ {
		want := fmt.Sprintf("cmd-%d-aaaaaa", 1000+i)
		found := false
		for _, d := range dropped {
			if d == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected oldest command %s to be dropped, got %v", want, dropped)
		}
	}
}

func TestPruneCommandsUnderCapIsNoOp(t *testing.T) {
	var specs []api.ModuleSpec
	for i := 0; i < defaultCommandHistory; i++ {
		specs = append(specs, api.ModuleSpec{Name: fmt.Sprintf("cmd-%d-aaaaaa", 2000+i), Version: "1"})
	}
	kept, dropped := pruneCommands(specs, defaultCommandHistory)
	if len(dropped) != 0 {
		t.Errorf("nothing should be dropped at exactly the cap, got %v", dropped)
	}
	if len(kept) != len(specs) {
		t.Errorf("kept %d, want %d", len(kept), len(specs))
	}
}

// Ordering must come from the embedded timestamp, since the store gives no
// ordering guarantee on the spec slice.
func TestPruneCommandsIgnoresSliceOrder(t *testing.T) {
	var specs []api.ModuleSpec
	for i := defaultCommandHistory + 2; i >= 0; i-- { // newest first in the slice
		specs = append(specs, api.ModuleSpec{Name: fmt.Sprintf("cmd-%d-aaaaaa", 3000+i), Version: "1"})
	}
	_, dropped := pruneCommands(specs, defaultCommandHistory)
	if len(dropped) != 3 {
		t.Fatalf("dropped %d, want 3: %v", len(dropped), dropped)
	}
	for _, d := range dropped {
		if got := commandCreatedAt(d); got >= 3003 {
			t.Errorf("dropped %s (t=%d); only the three oldest should go", d, got)
		}
	}
}

func TestCommandCreatedAt(t *testing.T) {
	if got := commandCreatedAt("cmd-1786156697-5b4e9c"); got != 1786156697 {
		t.Errorf("got %d, want 1786156697", got)
	}
	if got := commandCreatedAt("dcgm"); got != 0 {
		t.Errorf("non-command should yield 0, got %d", got)
	}
}
