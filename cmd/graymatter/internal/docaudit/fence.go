package docaudit

import "strings"

// maskFenced returns content with every line inside a fenced code block
// replaced byte-for-byte by spaces (newlines preserved), so marker scanning
// never sees documentation that QUOTES managed-block syntax. Length and line
// structure are identical to the input, which keeps every byte offset and
// line number valid for callers that scan the masked text.
//
// Fence recognition follows the CommonMark rule that matters here: an opening
// or closing line whose first non-space characters are three or more
// backticks or tildes, with a closing fence of the same character and at
// least the opener's length. Indented (four-space) code blocks are not
// fenced regions under this rule and stay visible, which is correct — they
// cannot hide a marker-pairing hazard because markdown still parses markers
// inside them.
func maskFenced(content string) string {
	lines := strings.SplitAfter(content, "\n")
	var (
		inFence  bool
		fenceCh  byte
		fenceLen int
	)
	for i, ln := range lines {
		trimmed := strings.TrimLeft(ln, " \t")
		isDelim := false
		if len(trimmed) >= 3 {
			c := trimmed[0]
			if c == '`' || c == '~' {
				n := 0
				for n < len(trimmed) && trimmed[n] == c {
					n++
				}
				if n >= 3 {
					switch {
					case !inFence:
						inFence, fenceCh, fenceLen = true, c, n
						isDelim = true
					case c == fenceCh && n >= fenceLen:
						inFence = false
						isDelim = true
					}
				}
			}
		}
		if inFence && !isDelim {
			lines[i] = maskLine(ln)
		}
	}
	return strings.Join(lines, "")
}

func maskLine(ln string) string {
	b := []byte(ln)
	for i, c := range b {
		if c != '\n' && c != '\r' {
			b[i] = ' '
		}
	}
	return string(b)
}
