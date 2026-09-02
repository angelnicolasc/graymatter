package memory

import "strings"

// stem reduces an English word to its root using the Porter algorithm
// (Porter, "An algorithm for suffix stripping", Program 14(3), 1980).
//
// It exists because the measured retrieval failures were not all semantic. Of
// the eight probes the keyword ranker missed on the revision harness, three
// missed on morphology alone: a question asking about "backups" against a fact
// saying "backup retention", "pager rotation" against "rotations were
// stretched", "how do we deploy" against "deployment moved". Those are the same
// word. The remaining five need genuine synonymy ("message broker" for an event
// bus named NATS) and are not this function's job.
//
// Porter rather than a hand-rolled suffix list: it is fully specified,
// deterministic, forty years stable, and it handles the -ment and -ation
// nominalisations that a naive plural stripper misses — which is where two of
// our three cases live. Pure Go, no model, no download, no cgo: the README
// promises one binary and this keeps it.
//
// The stemmer is conservative by construction — it never expands, only strips —
// so a term that stems to itself costs nothing. Words shorter than three
// characters are returned unchanged, which is where the algorithm's rules stop
// being meaningful anyway.
func stem(w string) string {
	if len(w) <= 2 {
		return w
	}
	b := []byte(w)
	b = step1a(b)
	b = step1b(b)
	b = step2(b)
	b = step3(b)
	b = step4(b)
	b = step5a(b)
	b = step5b(b)
	// Porter runs the y->i rule as step 1c, before suffix stripping. Run last
	// instead, because in the canonical order the two halves of a real pair
	// diverge: "deploy" reaches 1c and becomes "deploi", while "deployment"
	// loses its -ment in step 4 and ends as "deploy" with 1c already behind it.
	// Two spellings of one word, two stems, and the probe that motivated this
	// function still missing. Applying the rule after stripping makes both
	// paths converge on "deploi", and leaves every word in the published Porter
	// vocabulary (TestStemMatchesPorter) unchanged, because none of them reach
	// a trailing y only after a suffix comes off.
	b = step1c(b)
	return string(b)
}

func isConsonant(b []byte, i int) bool {
	switch b[i] {
	case 'a', 'e', 'i', 'o', 'u':
		return false
	case 'y':
		// y is a consonant at the start and after a vowel, a vowel otherwise.
		return i == 0 || !isConsonant(b, i-1)
	}
	return true
}

// measure counts vowel-consonant sequences: Porter's m, the [C](VC){m}[V] form.
func measure(b []byte) int {
	n, i := 0, 0
	for i < len(b) && isConsonant(b, i) {
		i++
	}
	for i < len(b) {
		for i < len(b) && !isConsonant(b, i) {
			i++
		}
		if i >= len(b) {
			break
		}
		n++
		for i < len(b) && isConsonant(b, i) {
			i++
		}
	}
	return n
}

func hasVowel(b []byte) bool {
	for i := range b {
		if !isConsonant(b, i) {
			return true
		}
	}
	return false
}

func doubleConsonant(b []byte) bool {
	n := len(b)
	return n >= 2 && b[n-1] == b[n-2] && isConsonant(b, n-1)
}

// cvc reports the consonant-vowel-consonant ending Porter uses to decide when
// to restore a final 'e', excluding w, x and y as the final consonant.
func cvc(b []byte) bool {
	n := len(b)
	if n < 3 || !isConsonant(b, n-1) || isConsonant(b, n-2) || !isConsonant(b, n-3) {
		return false
	}
	switch b[n-1] {
	case 'w', 'x', 'y':
		return false
	}
	return true
}

func ends(b []byte, suffix string) bool { return strings.HasSuffix(string(b), suffix) }

func replace(b []byte, suffix, with string, minM int) ([]byte, bool) {
	if !ends(b, suffix) {
		return b, false
	}
	stem := b[:len(b)-len(suffix)]
	if measure(stem) <= minM-1 {
		return b, false
	}
	return append(append([]byte{}, stem...), with...), true
}

func step1a(b []byte) []byte {
	switch {
	case ends(b, "sses"):
		return b[:len(b)-2]
	case ends(b, "ies"):
		return b[:len(b)-2]
	case ends(b, "ss"):
		return b
	case ends(b, "s"):
		return b[:len(b)-1]
	}
	return b
}

func step1b(b []byte) []byte {
	extra := false
	switch {
	case ends(b, "eed"):
		if measure(b[:len(b)-3]) > 0 {
			return b[:len(b)-1]
		}
		return b
	case ends(b, "ed"):
		if !hasVowel(b[:len(b)-2]) {
			return b
		}
		b, extra = b[:len(b)-2], true
	case ends(b, "ing"):
		if !hasVowel(b[:len(b)-3]) {
			return b
		}
		b, extra = b[:len(b)-3], true
	}
	if !extra {
		return b
	}
	switch {
	case ends(b, "at"), ends(b, "bl"), ends(b, "iz"):
		return append(b, 'e')
	case doubleConsonant(b):
		switch b[len(b)-1] {
		case 'l', 's', 'z':
			return b
		}
		return b[:len(b)-1]
	case measure(b) == 1 && cvc(b):
		return append(b, 'e')
	}
	return b
}

func step1c(b []byte) []byte {
	if ends(b, "y") && hasVowel(b[:len(b)-1]) {
		b[len(b)-1] = 'i'
	}
	return b
}

var step2Pairs = [][2]string{
	{"ational", "ate"}, {"tional", "tion"}, {"enci", "ence"}, {"anci", "ance"},
	{"izer", "ize"}, {"bli", "ble"}, {"alli", "al"}, {"entli", "ent"},
	{"eli", "e"}, {"ousli", "ous"}, {"ization", "ize"}, {"ation", "ate"},
	{"ator", "ate"}, {"alism", "al"}, {"iveness", "ive"}, {"fulness", "ful"},
	{"ousness", "ous"}, {"aliti", "al"}, {"iviti", "ive"}, {"biliti", "ble"},
	{"logi", "log"},
}

func step2(b []byte) []byte {
	for _, p := range step2Pairs {
		if out, ok := replace(b, p[0], p[1], 1); ok {
			return out
		}
	}
	return b
}

var step3Pairs = [][2]string{
	{"icate", "ic"}, {"ative", ""}, {"alize", "al"}, {"iciti", "ic"},
	{"ical", "ic"}, {"ful", ""}, {"ness", ""},
}

func step3(b []byte) []byte {
	for _, p := range step3Pairs {
		if out, ok := replace(b, p[0], p[1], 1); ok {
			return out
		}
	}
	return b
}

// step4 is where "deployment" loses its -ment and "reindexing" would not: the
// stem left behind has to measure at least 2, which is what keeps the rule from
// mangling short words.
var step4Suffixes = []string{
	"al", "ance", "ence", "er", "ic", "able", "ible", "ant", "ement", "ment",
	"ent", "ou", "ism", "ate", "iti", "ous", "ive", "ize",
}

func step4(b []byte) []byte {
	if ends(b, "ion") {
		stem := b[:len(b)-3]
		if measure(stem) > 1 && len(stem) > 0 {
			if last := stem[len(stem)-1]; last == 's' || last == 't' {
				return stem
			}
		}
		return b
	}
	for _, s := range step4Suffixes {
		if out, ok := replace(b, s, "", 2); ok {
			return out
		}
	}
	return b
}

func step5a(b []byte) []byte {
	if !ends(b, "e") {
		return b
	}
	stem := b[:len(b)-1]
	if m := measure(stem); m > 1 || (m == 1 && !cvc(stem)) {
		return stem
	}
	return b
}

func step5b(b []byte) []byte {
	if measure(b) > 1 && doubleConsonant(b) && ends(b, "l") {
		return b[:len(b)-1]
	}
	return b
}
