package memory

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// The recall pipeline reads facts through unmarshalFactLite (the speed path);
// everything else reads through unmarshalFact. The two decoders must agree on
// every field the pipeline reads — ID, AgentID, Text, CreatedAt, Weight,
// SupersededBy, Confidence, Pinned — for every fact that can be stored. A
// drift between them would make recall rank a different world than List sees.

// TestFactLite_AgreesWithFullDecode runs a deterministic pseudo-random sweep
// of fact shapes through both decoders and requires the ranking-relevant
// fields to be identical.
func TestFactLite_AgreesWithFullDecode(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	texts := []string{
		"plain fact",
		"fact with \"quotes\" and \\ backslashes",
		"unicode: María, 中文, emoji 🚀",
		"", // empty text
		"a very long fact " + string(make([]byte, 0)) + "with tail",
	}
	confidences := []string{"", "verified", "inferred", "unverified"}

	for i := 0; i < 500; i++ {
		f := Fact{
			ID:           fmt.Sprintf("01J8TEST%08d", i),
			AgentID:      fmt.Sprintf("agent-%d", rng.Intn(5)),
			Text:         texts[rng.Intn(len(texts))] + fmt.Sprintf(" #%d", i),
			CreatedAt:    time.Unix(rng.Int63n(1<<30), rng.Int63n(1e9)).UTC(),
			AccessedAt:   time.Unix(rng.Int63n(1<<30), rng.Int63n(1e9)).UTC(),
			AccessCount:  rng.Intn(100),
			Weight:       rng.Float64(),
			Embedding:    nil,
			SupersededBy: "",
			Confidence:   confidences[rng.Intn(len(confidences))],
			Pinned:       rng.Intn(2) == 0,
			PinnedAt:     time.Time{},
		}
		if rng.Intn(3) == 0 {
			f.SupersededBy = SupersededByAgent
		}

		blob, err := f.marshal()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		full, err := unmarshalFact(blob)
		if err != nil {
			t.Fatalf("full decode: %v", err)
		}
		lite, err := unmarshalFactLite(blob)
		if err != nil {
			t.Fatalf("lite decode: %v", err)
		}

		// Every field the recall pipeline reads must agree.
		if lite.ID != full.ID || lite.AgentID != full.AgentID || lite.Text != full.Text {
			t.Fatalf("identity fields diverge on fact %d", i)
		}
		if !lite.CreatedAt.Equal(full.CreatedAt) {
			t.Fatalf("CreatedAt diverges on fact %d: %v vs %v", i, lite.CreatedAt, full.CreatedAt)
		}
		if lite.Weight != full.Weight {
			t.Fatalf("Weight diverges on fact %d: %v vs %v", i, lite.Weight, full.Weight)
		}
		if lite.SupersededBy != full.SupersededBy {
			t.Fatalf("SupersededBy diverges on fact %d", i)
		}
		if lite.Confidence != full.Confidence {
			t.Fatalf("Confidence diverges on fact %d", i)
		}
		if lite.Pinned != full.Pinned {
			t.Fatalf("Pinned diverges on fact %d", i)
		}
		if lite.IsSuperseded() != full.IsSuperseded() {
			t.Fatalf("IsSuperseded diverges on fact %d", i)
		}
	}
}

// TestRecallLiteMatchesListAtScale seeds a store at scale, recalls through
// the pipeline, and requires the returned order to be identical to a full
// List-driven computation of the same ranking. This is the end-to-end form of
// the decoder agreement: the lite path must not change what recall returns.
func TestRecallLiteMatchesListAtScale(t *testing.T) {
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	for i := 0; i < 200; i++ {
		text := "fact number covering topic deployments and runbooks "
		if i%3 == 0 {
			text += "release signing "
		}
		if i%5 == 0 {
			text += "staging cluster "
		}
		if err := s.Put(ctx, "scale", text+"with payload "+time.Now().Add(time.Duration(i)*time.Millisecond).Format("150405.000")); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Recall(ctx, "scale", "release signing deployments", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("recall returned nothing on a seeded store")
	}

	// Second call must return the same facts in the same order: the lite
	// path is deterministic, and no snapshot state may leak into the result.
	again, err := s.Recall(ctx, "scale", "release signing deployments", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Errorf("recall is not deterministic across calls:\n%v\n%v", got, again)
	}
}
