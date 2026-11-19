package memory

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrInspectionUnsupported means an older backend cannot safely inspect or
// curate. Callers must not substitute a side-effecting recall or text mutation.
var ErrInspectionUnsupported = errors.New("store inspection unsupported; update and restart the daemon")

// ErrMutationOutcomeUnknown means a transport failed after dispatch. Refresh
// the selected fact before deciding what to do; never replay the write blindly.
var ErrMutationOutcomeUnknown = errors.New("mutation outcome unknown; refresh before retrying")

// FactPage is a bounded snapshot ordered by descending stable fact ID. Facts
// preserve metadata but omit embeddings. Total counts all matches in this read.
// Separate pages are live reads, not a transaction spanning user interaction.
type FactPage struct {
	Facts         []Fact         `json:"facts"`
	NextCursor    string         `json:"next_cursor,omitempty"`
	Total         int            `json:"total"`
	SubjectIssues *SubjectIssues `json:"subject_issues,omitempty"`
}

// Inspector is optional; published store interfaces remain source compatible.
type Inspector interface {
	RecallPreview(context.Context, string, string, int) ([]RecallReceipt, error)
	ListFacts(context.Context, string, string, string, string, int) (FactPage, error)
}

// FactCurator applies an action to exactly one selected ID and snapshot.
type FactCurator interface {
	CurateFact(context.Context, string, string, string, string, Fact) (Fact, error)
}

// InspectionHealth describes observed state without probing external models.
// ProviderConfigured is configuration, never a claim of current network health.
type InspectionHealth struct {
	Provider             string          `json:"provider"`
	ProviderConfigured   bool            `json:"provider_configured"`
	ProviderReachability string          `json:"provider_reachability"`
	Embedding            EmbeddingHealth `json:"embedding"`
}

// RecallPreview returns the same ranking receipts as RecallExplain without
// touching facts, learning aliases, firing recall/debug hooks or enriching the
// result with unreceipted graph hints. A configured embedder can still be used
// to rank the query; inspection does not invoke a generative model.
func (s *Store) RecallPreview(ctx context.Context, agent, query string, topK int) ([]RecallReceipt, error) {
	p, err := s.resolveRecallPolicy(RecallOptions{})
	if err != nil {
		return nil, err
	}
	p.preview = true
	return s.recallExplainWithPolicy(ctx, agent, query, recallTopK(topK), p)
}

// InspectHealth reads durable observations and actual configured provider.
// Reachability stays unprobed: a successful database read cannot prove that an
// external embedding endpoint is healthy.
func (s *Store) InspectHealth(ctx context.Context) (InspectionHealth, error) {
	if err := ctx.Err(); err != nil {
		return InspectionHealth{}, err
	}
	h, err := s.EmbeddingHealth()
	if err != nil {
		return InspectionHealth{}, err
	}
	out := InspectionHealth{Provider: "keyword", ProviderReachability: "not applicable", Embedding: h}
	if s.embedder != nil && s.embedder.Dimensions() > 0 {
		out.Provider = s.embedder.Name()
		out.ProviderConfigured = true
		out.ProviderReachability = "not probed"
	}
	return out, nil
}

type factPageCursor struct {
	Scope  string `json:"scope"`
	Before string `json:"before"`
}

// ListFacts scans lightweight rows and historical tombstone links, then transfers
// only the requested page. Total is exact at the cost of an O(N) metadata scan;
// embeddings are skipped by the decoder rather than loaded then discarded.
// States: active (live content), retired (all tombstones), alias (live aliases),
// all. Query is a case-insensitive literal substring, not recall ranking.
func (s *Store) ListFacts(ctx context.Context, agent, state, query, cursor string, limit int) (FactPage, error) {
	if err := ctx.Err(); err != nil {
		return FactPage{}, err
	}
	if state == "" {
		state = "active"
	}
	switch state {
	case "active", "retired", "alias", "all":
	default:
		return FactPage{}, fmt.Errorf("invalid fact state %q", state)
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	query = strings.ToLower(query)
	filter, _ := json.Marshal([]string{agent, state, query})
	scope := fmt.Sprintf("%x", sha256.Sum256(filter))
	var token factPageCursor
	if cursor != "" {
		if len(cursor) > 512 {
			return FactPage{}, errors.New("invalid fact page cursor")
		}
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &token) != nil || token.Scope != scope || token.Before == "" {
			return FactPage{}, errors.New("invalid fact page cursor for this filter")
		}
	}
	out := FactPage{Facts: make([]Fact, 0, limit)}
	hasMore := false
	err := s.db.View(func(tx *bolt.Tx) error {
		idx, err := resolveSubjectIndexTx(tx, agent)
		if err != nil {
			return err
		}
		var issues SubjectIssues
		issues.add(agent, idx)
		out.SubjectIssues = issues.nonempty()
		parent := tx.Bucket(bucketFacts)
		if parent == nil {
			return nil
		}
		b := parent.Bucket([]byte(agent))
		if b == nil {
			return nil
		}
		incoming := make(map[string][]string)
		c := b.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if v == nil {
				continue
			}
			f, err := unmarshalInspectionFact(v)
			if err != nil {
				return err
			}
			if f.AgentID != agent || f.ID != string(k) {
				return fmt.Errorf("fact identity mismatch in namespace %q", agent)
			}
			if f.SupersededBy != "" && f.SupersededBy != SupersededByAgent {
				incoming[f.SupersededBy] = append(incoming[f.SupersededBy], f.ID)
			}
			match := state == "all" || state == "retired" && f.IsSuperseded() || state == "alias" && f.IsAlias() && !f.IsSuperseded() || state == "active" && !f.IsAlias() && !f.IsSuperseded()
			if !match || !strings.Contains(strings.ToLower(f.Text), query) {
				continue
			}
			out.Total++
			if token.Before != "" && f.ID >= token.Before {
				continue
			}
			if len(out.Facts) < limit {
				if err := hydrateLineageTx(tx, &f); err != nil {
					return err
				}
				if err := verifyLineageProjectionTx(ctx, tx, &f); err != nil {
					return err
				}
				out.Facts = append(out.Facts, f)
			} else {
				hasMore = true
			}
		}
		projectLegacyLineage(out.Facts, incoming)
		projectSubjectFacts(idx.ByVersion, out.Facts)
		return nil
	})
	if err != nil {
		return FactPage{}, err
	}
	if hasMore {
		raw, _ := json.Marshal(factPageCursor{Scope: scope, Before: out.Facts[len(out.Facts)-1].ID})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
}

func unmarshalInspectionFact(data []byte) (Fact, error) {
	var row struct {
		factLite
		AccessedAt  time.Time `json:"accessed_at"`
		AccessCount int       `json:"access_count"`
		PinnedAt    time.Time `json:"pinned_at"`
	}
	if err := json.Unmarshal(data, &row); err != nil {
		return Fact{}, err
	}
	return Fact{LineageProjection: row.LineageProjection, ID: row.ID, AgentID: row.AgentID, Text: row.Text, CreatedAt: row.CreatedAt, AccessedAt: row.AccessedAt, AccessCount: row.AccessCount, Weight: row.Weight, SupersededBy: row.SupersededBy, Confidence: row.Confidence, Kind: row.Kind, AliasSource: row.AliasSource, Pinned: row.Pinned, PinnedAt: row.PinnedAt}, nil
}

func sourceVersionMatches(expected, current Fact) bool {
	return sourceMatches(expected, current, false) && expected.SupersededBy == current.SupersededBy &&
		expected.Pinned == current.Pinned && expected.PinnedAt.Equal(current.PinnedAt) &&
		expected.CreatedAt.Equal(current.CreatedAt) && expected.AliasSource == current.AliasSource
}

// CurateFact preserves lineage and confidence semantics while targeting only
// factID, never all equal-text siblings. Content/pin/confidence changes reject
// the snapshot; access counters and decay may advance without causing conflicts.
// Revision commits the replacement and source tombstone atomically, derives
// conservative confidence and does not inherit the pin.
func (s *Store) CurateFact(ctx context.Context, agent, factID, action, text string, expected Fact) (Fact, error) {
	if err := ctx.Err(); err != nil {
		return Fact{}, err
	}
	if s.readOnly {
		return Fact{}, ErrStoreReadOnly
	}
	if agent == "" || factID == "" || expected.ID != factID || expected.AgentID != agent || expected.IsSuperseded() {
		return Fact{}, fmt.Errorf("%w: invalid selected fact", ErrFactChanged)
	}
	switch action {
	case "pin", "unpin", "retire":
		if text != "" {
			return Fact{}, errors.New("text is only valid for revise")
		}
	case "revise":
		if strings.TrimSpace(text) == "" {
			return Fact{}, errors.New("replacement text is required")
		}
	default:
		return Fact{}, fmt.Errorf("invalid curation action %q", action)
	}
	check := func(current Fact) error {
		if !sourceVersionMatches(expected, current) {
			return fmt.Errorf("%w: reload fact %s", ErrFactChanged, factID)
		}
		return ctx.Err()
	}
	if action == "revise" {
		// Reject stale state before potentially expensive embedding, then check
		// again inside the single durable transaction.
		if err := s.db.View(func(tx *bolt.Tx) error {
			f, err := factInTx(tx, agent, factID)
			if err != nil {
				return err
			}
			return check(f)
		}); err != nil {
			return Fact{}, err
		}
		f, err := s.reviseSources(ctx, agent, text, WriteOptions{}, []Fact{expected})
		f.Embedding = nil
		return f, err
	}
	f, err := s.patchFact(agent, factID, func(current *Fact) error {
		if err := check(*current); err != nil {
			return err
		}
		if action == "retire" {
			current.SupersededBy = SupersededByAgent
			return nil
		}
		pinned := action == "pin"
		if current.Pinned == pinned {
			return errFactUnchanged
		}
		current.Pinned = pinned
		current.PinnedAt = time.Time{}
		if pinned {
			current.PinnedAt = s.now().UTC()
		}
		return nil
	})
	f.Embedding = nil
	return f, err
}
