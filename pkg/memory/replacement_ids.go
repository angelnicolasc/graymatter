package memory

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// ReplacementIDs holds an ordered list of fact IDs by value. The zero value
// omits explicit replacements. Use NewReplacementIDs to snapshot a list.
type ReplacementIDs struct {
	encoded string
}

// NewReplacementIDs snapshots ids. A nil list omits replacements. An
// explicitly empty list remains present and fails write validation.
func NewReplacementIDs(ids []string) (ReplacementIDs, error) {
	if ids == nil {
		return ReplacementIDs{}, nil
	}
	for _, id := range ids {
		if !utf8.ValidString(id) {
			return ReplacementIDs{}, fmt.Errorf("replaces must contain non-empty UTF-8 fact IDs")
		}
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return ReplacementIDs{}, err
	}
	return ReplacementIDs{encoded: string(raw)}, nil
}

// Present distinguishes explicit replacement intent from omission.
func (r ReplacementIDs) Present() bool { return r.encoded != "" }

// IDs returns an independent copy of the ordered IDs. It returns nil for an
// omitted list.
func (r ReplacementIDs) IDs() []string {
	if !r.Present() {
		return nil
	}
	var ids []string
	// The constructor is the only writer of the encoded array.
	_ = json.Unmarshal([]byte(r.encoded), &ids)
	return ids
}

// MarshalJSON encodes the ordered IDs as a JSON array or null for omission.
func (r ReplacementIDs) MarshalJSON() ([]byte, error) {
	if !r.Present() {
		return []byte("null"), nil
	}
	return []byte(r.encoded), nil
}

func (r *ReplacementIDs) UnmarshalJSON(data []byte) error {
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return err
	}
	value, err := NewReplacementIDs(ids)
	if err != nil {
		return err
	}
	*r = value
	return nil
}

// Keep the existing JSON array on the wire while Go callers retain a
// comparable WriteOptions value.
func (o WriteOptions) MarshalJSON() ([]byte, error) {
	type plain WriteOptions
	wire := struct {
		plain
		Replaces json.RawMessage `json:"replaces,omitempty"`
	}{plain: plain(o)}
	if o.Replaces.Present() && o.Replaces.encoded != "[]" {
		wire.Replaces = json.RawMessage(o.Replaces.encoded)
	}
	return json.Marshal(wire)
}

func (o *WriteOptions) UnmarshalJSON(data []byte) error {
	type plain WriteOptions
	value := plain(*o)
	wire := struct {
		*plain
		Replaces ReplacementIDs `json:"replaces"`
	}{plain: &value, Replaces: o.Replaces}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	value.Replaces = wire.Replaces
	*o = WriteOptions(value)
	return nil
}
