package memory

import (
	"encoding/json"
	"slices"
	"testing"
)

func testReplacementIDs(t *testing.T, ids ...string) ReplacementIDs {
	t.Helper()
	value, err := NewReplacementIDs(ids)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestWriteOptionsComparableAndReplacementWireFormat(t *testing.T) {
	label := "verified"
	ids := []string{"fact-é", "fact-2"}
	replaces, err := NewReplacementIDs(ids)
	if err != nil {
		t.Fatal(err)
	}
	options := WriteOptions{Confidence: &label, Replaces: replaces}
	same := WriteOptions{Confidence: &label, Replaces: testReplacementIDs(t, "fact-é", "fact-2")}
	byOption := map[WriteOptions]bool{options: true}
	if options != same || !byOption[same] {
		t.Fatal("equivalent replacement lists must compare by value")
	}
	ids[0] = "caller-changed"
	values := options.Replaces.IDs()
	values[1] = "reader-changed"
	if options != same || !slices.Equal(options.Replaces.IDs(), []string{"fact-é", "fact-2"}) {
		t.Fatal("mutation escaped a replacement-list copy")
	}
	data, err := json.Marshal(options)
	if err != nil || string(data) != `{"confidence":"verified","replaces":["fact-é","fact-2"]}` {
		t.Fatalf("unexpected wire options: %s (%v)", data, err)
	}
	list, err := json.Marshal(replaces)
	if err != nil || string(list) != `["fact-é","fact-2"]` {
		t.Fatalf("unexpected replacement list: %s (%v)", list, err)
	}
	var decoded WriteOptions
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.Replaces != same.Replaces {
		t.Fatalf("wire replacements changed: %v (%v)", decoded.Replaces.IDs(), err)
	}
	data, err = json.Marshal(WriteOptions{Confidence: &label})
	if err != nil || string(data) != `{"confidence":"verified"}` {
		t.Fatalf("ordinary option wire format changed: %s (%v)", data, err)
	}
	reordered := testReplacementIDs(t, "fact-2", "fact-é")
	if reordered == same.Replaces {
		t.Fatal("replacement order must be preserved in comparison")
	}
	if err := (WriteOptions{Replaces: testReplacementIDs(t, "fact-2", "fact-2")}).Validate(); err == nil {
		t.Fatal("duplicate replacement IDs passed write validation")
	}
	if err := json.Unmarshal([]byte(`{"replaces":{"id":"fact-2"}}`), &decoded); err == nil {
		t.Fatal("non-array replacement IDs decoded successfully")
	}
	var empty WriteOptions
	if err := json.Unmarshal([]byte(`{"replaces":[]}`), &empty); err != nil || !empty.Replaces.Present() || empty.Validate() == nil {
		t.Fatalf("explicit empty replacements must fail write validation: %+v (%v)", empty, err)
	}
	if err := json.Unmarshal([]byte(`{"replaces":null}`), &decoded); err != nil || decoded.Replaces.Present() {
		t.Fatalf("decoded null replacements were not cleared: %+v (%v)", decoded, err)
	}
}
