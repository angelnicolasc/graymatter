package daemon

import (
	"os"
	"path/filepath"
)

// KGSentinelFile is the marker `graymatter init --kg` drops into the data
// directory to request knowledge-graph auto-population for every future
// daemon — spawned automatically by MCP clients (which launch their servers
// with their own environment, so an exported GRAYMATTER_KG in a shell does
// not reach them) or run by hand. It is runtime state written by one of our
// commands, not user-authored configuration: same statute as the HTTP token
// file, and no change to the no-config-files promise.
const KGSentinelFile = "kg.auto"

// KGSentinelPath returns the sentinel's location inside dataDir.
func KGSentinelPath(dataDir string) string {
	return filepath.Join(dataDir, KGSentinelFile)
}

// kgAutoEnabled decides whether this daemon wires extraction into
// consolidation. OR-semantics, checked in one place so manual runs and
// client-spawned daemons can never disagree:
//
//   - --kg flag (explicit, strongest)
//   - GRAYMATTER_KG=1 in the daemon's own environment
//   - the kg.auto sentinel left by `graymatter init --kg`
//
// The engine contract is untouched either way: SetKG stays an explicit call
// the daemon makes here, and pkg/memory's wiring-contract test keeps pinning
// that shipped defaults never auto-wire on their own.
func kgAutoEnabled(dataDir string, flagKG bool) bool {
	if flagKG || os.Getenv("GRAYMATTER_KG") == "1" {
		return true
	}
	_, err := os.Stat(KGSentinelPath(dataDir))
	return err == nil
}
