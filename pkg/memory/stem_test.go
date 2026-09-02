package memory

import "testing"

// The three cases that motivated the stemmer, taken verbatim from the probes
// that failed on the revision harness. If these stop collapsing, the reason the
// function exists is gone.
func TestStemCollapsesTheMeasuredFailures(t *testing.T) {
	pairs := [][2]string{
		{"backups", "backup"},     // "how long do we keep backups" vs "backup retention"
		{"rotation", "rotations"}, // "pager rotation" vs "rotations were stretched"
		{"deploy", "deployment"},  // "how do we deploy" vs "deployment moved"
		{"payment", "payments"},   // "payment gateway" vs "payments gateway integration"
	}
	for _, p := range pairs {
		if a, b := stem(p[0]), stem(p[1]); a != b {
			t.Errorf("stem(%q)=%q but stem(%q)=%q — they must collapse", p[0], a, p[1], b)
		}
	}
}

// Porter's own published behaviour, so a future edit cannot quietly turn this
// into a different algorithm.
func TestStemMatchesPorter(t *testing.T) {
	cases := map[string]string{
		"caresses": "caress", "ponies": "poni", "ties": "ti", "caress": "caress",
		"cats": "cat", "feed": "feed", "agreed": "agre", "plastered": "plaster",
		"bled": "bled", "motoring": "motor", "sing": "sing", "conflated": "conflat",
		"troubling": "troubl", "sized": "size", "hopping": "hop", "falling": "fall",
		"hissing": "hiss", "fizzed": "fizz", "failing": "fail", "filing": "file",
		"happy": "happi", "sky": "sky", "relational": "relat",
		"conditional": "condit", "rational": "ration", "valenci": "valenc",
		"hesitanci": "hesit", "digitizer": "digit", "conformabli": "conform",
		"radicalli": "radic", "differentli": "differ", "vileli": "vile",
		"analogousli": "analog", "vietnamization": "vietnam", "predication": "predic",
		"operator": "oper", "feudalism": "feudal", "decisiveness": "decis",
		"hopefulness": "hope", "callousness": "callous", "formaliti": "formal",
		"sensitiviti": "sensit", "sensibiliti": "sensibl", "triplicate": "triplic",
		"formative": "form", "formalize": "formal", "electriciti": "electr",
		"electrical": "electr", "hopeful": "hope", "goodness": "good",
		"revival": "reviv", "allowance": "allow", "inference": "infer",
		"airliner": "airlin", "gyroscopic": "gyroscop", "adjustable": "adjust",
		"defensible": "defens", "irritant": "irrit", "replacement": "replac",
		"adjustment": "adjust", "dependent": "depend", "adoption": "adopt",
		"homologou": "homolog", "communism": "commun", "activate": "activ",
		"angulariti": "angular", "homologous": "homolog", "effective": "effect",
		"bowdlerize": "bowdler", "probate": "probat", "rate": "rate",
		"cease": "ceas", "controll": "control", "roll": "roll",
	}
	for in, want := range cases {
		if got := stem(in); got != want {
			t.Errorf("stem(%q) = %q, want %q", in, got, want)
		}
	}
}

// Short inputs and edge shapes must not panic or mangle.
func TestStemShortAndOddInputs(t *testing.T) {
	for _, s := range []string{"", "a", "ab", "abc", "s", "ss", "sss", "ies", "eed", "y", "yy"} {
		got := stem(s)
		if len(s) <= 2 && got != s {
			t.Errorf("stem(%q) = %q; inputs of 2 chars or fewer are returned unchanged", s, got)
		}
		if len(got) > len(s)+1 {
			t.Errorf("stem(%q) = %q — the stemmer strips, it does not expand", s, got)
		}
	}
}

// Porter is deliberately NOT idempotent on arbitrary strings: "supersedes"
// stems to "supersed", and feeding that back in strips the -ed to "supers",
// because "supersed" is an intermediate, not a word anyone writes. Asserting
// idempotence would be asserting a property the algorithm does not have.
//
// The invariant that actually matters is narrower and is the one the pipeline
// depends on: stemming happens exactly once, in tokenize, on both the query and
// the stored text. So the test is that the two sides agree — which is the only
// way the signal can work — not that stemming survives being applied twice.
func TestQueryAndFactStemToTheSameTerms(t *testing.T) {
	cases := [][2]string{
		{"how long do we keep backups?", "backup retention was extended to 91 days"},
		{"how long is a pager rotation?", "rotations were stretched to a fortnight"},
		{"how do we deploy to Kubernetes?", "deployment moved to Kustomize overlays"},
		{"who signs off on payment gateway changes?",
			"Marta Oliveira owns the payments gateway integration and reviews every change to it"},
	}
	for _, c := range cases {
		q, f := tokenizeStem(c[0], true), tokenizeStem(c[1], true)
		shared := map[string]bool{}
		for _, a := range q {
			for _, b := range f {
				if a == b {
					shared[a] = true
				}
			}
		}
		if len(shared) == 0 {
			t.Errorf("no shared term after stemming:\n  query %v\n  fact  %v", q, f)
		}
	}
}
