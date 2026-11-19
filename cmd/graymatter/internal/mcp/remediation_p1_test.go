package mcp

import "testing"

func TestRP1ExactCoreInstructions(t *testing.T) {
	// T3 changes the empty-store directive and the instructions for durable writes.
	const want = coreServerInstructions
	const empty = emptyCoreServerInstructions
	base, _ := utilityContractServer(t)
	if got := initializeInstructions(t, base); got != want {
		t.Errorf("core instructions changed: %q", got)
	}
	local, _ := utilityContractServer(t, WithLocalProjectStartup("contingency-core"))
	if got := initializeInstructions(t, local); got != empty {
		t.Errorf("empty core instructions changed: %q", got)
	}
	vcSavePendingCheckpoint(t, local, "contingency-core")
	if got := initializeInstructions(t, local); got != want {
		t.Errorf("pending core instructions changed: %q", got)
	}
}
