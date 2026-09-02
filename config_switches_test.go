package graymatter

import "testing"

// The retrieval switches had no surface outside StoreConfig, so the only way
// to reach them was to import the library and build a Config in Go — which the
// CLI, the daemon, the MCP server and the REST server all refuse to do (they
// build from DefaultConfig, deliberately, so there is one place that decides
// what a store is). A knob nobody can turn is a knob that does not exist; this
// pins that DefaultConfig is that place.
//
// UsageAliasLearning is the one switch that is still opt-in: measured against
// real agent reformulations it is worth +2 families out of 40 and promotes
// about one junk pair in three, so only an explicit yes may turn it on.
func TestRetrievalSwitchesReadTheEnvironment(t *testing.T) {
	const key = "GRAYMATTER_USAGE_ALIAS"
	if DefaultConfig().UsageAliasLearning {
		t.Fatal("UsageAliasLearning defaults to true with the variable unset, want false")
	}
	// The switch changes what the store promotes from observed queries, so
	// the default has to survive anything that is not a deliberate yes.
	for _, off := range []string{"", "0", "false", "no", "off", "maybe", " "} {
		t.Setenv(key, off)
		if DefaultConfig().UsageAliasLearning {
			t.Errorf("%s=%q turned UsageAliasLearning on; only an explicit yes may", key, off)
		}
	}
	for _, on := range []string{"1", "true", "TRUE", "yes", "on", " 1 "} {
		t.Setenv(key, on)
		if !DefaultConfig().UsageAliasLearning {
			t.Errorf("%s=%q did not turn UsageAliasLearning on", key, on)
		}
	}
}

// The switches are independent, and they do not share a default. Usage-alias
// learning is off; stemming and candidate retrieval are on; and each has to
// stay where it is when the others move: measured on a second evaluation corpus, running
// the learner with the stemmer scored BELOW running the learner alone (9/10
// against 10/10), so a switch that silently dragged another along would ship a
// configuration nobody chose and nobody measured.
func TestRetrievalSwitchesAreIndependent(t *testing.T) {
	t.Setenv("GRAYMATTER_USAGE_ALIAS", "1")
	cfg := DefaultConfig()
	if !cfg.UsageAliasLearning {
		t.Fatal("GRAYMATTER_USAGE_ALIAS=1 did not enable usage-alias learning")
	}
	if !cfg.StemKeywords {
		t.Error("enabling usage-alias learning turned the stemmer off")
	}
	if !cfg.CandidateRetrieval {
		t.Error("enabling usage-alias learning turned candidate retrieval off")
	}

	t.Setenv("GRAYMATTER_USAGE_ALIAS", "")
	t.Setenv("GRAYMATTER_STEM_KEYWORDS", "0")
	cfg = DefaultConfig()
	if cfg.StemKeywords {
		t.Fatal("GRAYMATTER_STEM_KEYWORDS=0 did not opt out")
	}
	if cfg.UsageAliasLearning {
		t.Error("opting out of the stemmer turned usage-alias learning on")
	}
	if !cfg.CandidateRetrieval {
		t.Error("opting out of the stemmer turned candidate retrieval off")
	}

	t.Setenv("GRAYMATTER_STEM_KEYWORDS", "")
	t.Setenv("GRAYMATTER_CANDIDATE_RETRIEVAL", "0")
	cfg = DefaultConfig()
	if cfg.CandidateRetrieval {
		t.Fatal("GRAYMATTER_CANDIDATE_RETRIEVAL=0 did not opt out")
	}
	if !cfg.StemKeywords {
		t.Error("opting out of candidate retrieval turned the stemmer off")
	}
	if cfg.UsageAliasLearning {
		t.Error("opting out of candidate retrieval turned usage-alias learning on")
	}
}

// The affinity gate is the one retrieval knob that is not a boolean, and the
// owner's decision was explicit: the mechanism stays reachable, the default
// stays conservative. Both halves need pinning — a knob nobody can set is not
// "documented and available", and a default that drifts open would ship the
// pollution the measurement rejected.
func TestUsageAliasAffinityIsReachableAndDefaultsClosed(t *testing.T) {
	if got := DefaultConfig().UsageAliasAffinityMin; got != 0 {
		t.Fatalf("affinity defaults to %d; 0 is the value memory.Open normalises to the conservative gate", got)
	}
	t.Setenv("GRAYMATTER_USAGE_ALIAS_AFFINITY", "-1")
	if got := DefaultConfig().UsageAliasAffinityMin; got != -1 {
		t.Errorf("the synonym-class mode is unreachable from the environment: got %d", got)
	}
	// A typo must land on the documented default, not on a number nobody chose.
	for _, junk := range []string{"", "off", "yes", "-", "1.5"} {
		t.Setenv("GRAYMATTER_USAGE_ALIAS_AFFINITY", junk)
		if got := DefaultConfig().UsageAliasAffinityMin; got != 0 {
			t.Errorf("%q parsed to %d, want the 0 that means \"use the default\"", junk, got)
		}
	}
}

// StemKeywords is a retrieval switch that defaults ON, so it needs the
// mirror image of the opt-in test above: the default has to survive anything
// that is not a deliberate NO, and the deliberate no has to work.
//
// The second half is the part that would have shipped broken. envBool maps
// everything that is not an explicit yes to false, so a default-on flag built
// on it can be turned on and never off — "unset" and "0" give the same answer.
// That is the shape of every reachability bug this project has paid for.
func TestStemKeywordsDefaultsOnAndCanBeTurnedOff(t *testing.T) {
	if !DefaultConfig().StemKeywords {
		t.Fatal("stemming must default ON: measured +4/35 on a corpus it was not designed against, losing nothing")
	}
	for _, on := range []string{"", "1", "true", "yes", "on", "maybe", " "} {
		t.Setenv("GRAYMATTER_STEM_KEYWORDS", on)
		if !DefaultConfig().StemKeywords {
			t.Errorf("%q turned stemming off; only an explicit no may", on)
		}
	}
	for _, off := range []string{"0", "false", "no", "off", "OFF", " 0 "} {
		t.Setenv("GRAYMATTER_STEM_KEYWORDS", off)
		if DefaultConfig().StemKeywords {
			t.Errorf("GRAYMATTER_STEM_KEYWORDS=%q did not opt out; the flag is unreachable in the off direction", off)
		}
	}
}

// CandidateRetrieval is the other retrieval switch that defaults ON, so it
// gets the same mirror: the default has to survive anything that is not a
// deliberate NO, and the deliberate no has to work.
//
// The default was earned the way a default has to be — the 30k latency gate
// green on two machines (p99 20.4 ms and 21.2 ms against the 40 ms bar, with
// a harness that verifies every measured recall returned facts) and the
// write-cost bar ratified at <= 3 ms by the owner. It ships to everyone, so
// everyone must be able to turn it off: the reachability bug in the off
// direction is the one this project has paid for three times.
func TestCandidateRetrievalDefaultsOnAndCanBeTurnedOff(t *testing.T) {
	if !DefaultConfig().CandidateRetrieval {
		t.Fatal("candidate retrieval must default ON: latency gate green on two machines, write bar ratified")
	}
	for _, on := range []string{"", "1", "true", "yes", "on", "maybe", " "} {
		t.Setenv("GRAYMATTER_CANDIDATE_RETRIEVAL", on)
		if !DefaultConfig().CandidateRetrieval {
			t.Errorf("%q turned candidate retrieval off; only an explicit no may", on)
		}
	}
	for _, off := range []string{"0", "false", "no", "off", "OFF", " 0 "} {
		t.Setenv("GRAYMATTER_CANDIDATE_RETRIEVAL", off)
		if DefaultConfig().CandidateRetrieval {
			t.Errorf("GRAYMATTER_CANDIDATE_RETRIEVAL=%q did not opt out; the flag is unreachable in the off direction", off)
		}
	}
}
