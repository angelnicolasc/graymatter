package memory

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const MaxSubjectBytes = 256

// NormalizeSubject deliberately performs exact Unicode normalization only.
// Search tokenization, stemming and lexical similarity never select victims.
func NormalizeSubject(value string) (string, error) {
	if !utf8.ValidString(value) || len(value) > MaxSubjectBytes {
		return "", fmt.Errorf("subject must be valid UTF-8 and at most %d bytes", MaxSubjectBytes)
	}
	key := norm.NFC.String(strings.Join(strings.Fields(strings.ToLower(norm.NFC.String(value))), " "))
	if key == "" || len(key) > MaxSubjectBytes {
		return "", fmt.Errorf("normalized subject must contain 1..%d bytes", MaxSubjectBytes)
	}
	return key, nil
}

func (o WriteOptions) HasSubjectIntent() bool { return o.Subject != nil || o.Replaces.Present() }

func (o WriteOptions) validateSubjectOptions() error {
	replaces := o.Replaces.IDs()
	if o.Subject != nil {
		if replaces != nil && len(replaces) != 1 {
			return fmt.Errorf("subject and replaces are alternatives")
		}
		if _, err := NormalizeSubject(*o.Subject); err != nil {
			return err
		}
	}
	if replaces != nil {
		if len(replaces) == 0 {
			return fmt.Errorf("replaces must contain at least one fact ID")
		}
		seen := map[string]bool{}
		for _, id := range replaces {
			if strings.TrimSpace(id) == "" || !utf8.ValidString(id) {
				return fmt.Errorf("replaces must contain non-empty UTF-8 fact IDs")
			}
			if seen[id] {
				return fmt.Errorf("replaces repeats fact ID %q", id)
			}
			seen[id] = true
		}
	}
	return nil
}
