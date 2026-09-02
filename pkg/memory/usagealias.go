package memory

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Usage-alias learning: the store promotes its own
// vocabulary from observed behaviour, with no agent action and no server-side
// semantics. The signal is a reformulation pair — a weak match (the
// weak-match trigger fired) followed by a strong match from the same agent, where
// the strong query's new terms are vocabulary the store knows and the weak
// query's missing terms are vocabulary it does not. The LLM's reformulation IS
// the semantic decision; the store only counts co-occurrence evidence and
// promotes the pair on its second independent observation.
//
// Guardrails, pre-registered before this code existed:
//   - promotion threshold k=2: a pair seen once never promotes. Zero false
//     promotions from single observations is a test-pinned property.
//   - one pending miss per agent, expiring after pendingMissTTL: sessions are
//     minutes-scale, and a stale weak query must not pair with tomorrow's
//     unrelated strong one. The latest miss wins.
//   - no lexical-relatedness requirement between the weak and strong query:
//     a genuine reformulation can share zero tokens with the original (that
//     is what makes it a gap), so relatedness is supplied by the LLM's
//     reformulation, not by a token heuristic. The noise this admits is
//     bounded by the k=2 threshold and the df filters below.
//   - the weak term must still be unknown (df == 0) at pairing time and the
//     strong term must be known (df > 0): an alias teaches "your word for my
//     word", never the reverse.
//   - per event, each unknown term pairs only with the single most
//     distinctive working term (highest idf among the delta) — capping
//     promotion spam instead of wiring every pair the delta offers.
//   - pairs expire after pairMaxAge, pruned at Open: stale evidence must not
//     promote vocabulary months later.
//   - promoted aliases carry AliasSource="usage", distinct from
//     agent-written ones, so autonomy cannot masquerade as curation.
//   - everything here is off until StoreConfig.UsageAliasLearning is set:
//     like StemKeywords, it ships default-on only after its promotion and
//     false-promotion rates are measured on a held-out corpus rather than assumed.

const (
	// usageAliasThreshold is how many independent observations a
	// reformulation pair needs before the store promotes it (pre-registered
	// k=2). A single observation is a coincidence until repeated.
	usageAliasThreshold = 2
	// pendingMissTTL bounds how long a weak match waits for its strong
	// counterpart. Sessions are minutes-scale; half an hour is generous.
	pendingMissTTL = 30 * time.Minute
	// pairMaxAge is how long an unpromoted pair survives. Pruned at Open.
	pairMaxAge = 30 * 24 * time.Hour
	// AliasSourceUsage marks an alias the store promoted itself.
	AliasSourceUsage = "usage"
)

// pendingMiss is the content terms of the latest weak match for one agent.
type pendingMiss struct {
	Terms []string `json:"terms"`
	At    int64    `json:"at"`
}

// reformPair is the accumulated evidence for one vocabulary pair.
type reformPair struct {
	Count  int   `json:"count"`
	LastAt int64 `json:"last_at"`
}

// learnFromRecall is the usage-alias hook RecallDetailed runs after every recall.
// Weak matches leave a pending miss (the unknown, non-question terms only);
// strong matches consume the agent's pending miss, record the (unknown-word,
// working-word) evidence and, on the second observation of an affined pair,
// promote it to a usage alias. Best-effort and write-only: a learning failure
// must never break a read.
func (s *Store) learnFromRecall(ctx context.Context, agentID, query string, p *recallPipeline) {
	if !s.cfg.UsageAliasLearning || s.readOnly || p == nil {
		return
	}
	toks := dedupTokens(tokenize(query))
	if len(toks) == 0 {
		return
	}
	doStem := s.cfg.StemKeywords

	if p.feedback != "" {
		// Pending miss: only the terms that made the match weak — unknown to
		// the store (df == 0 under the scorer's fold) and not interrogative
		// scaffolding. "Who" was never going to be in the store; "paying"
		// might be, and that is the word worth learning.
		var unknown []string
		for _, t := range toks {
			if questionWords[t] {
				continue
			}
			stemmed := tokenizeStem(t, doStem)
			if len(stemmed) == 0 || p.df[stemmed[0]] > 0 {
				continue
			}
			unknown = append(unknown, t)
		}
		if len(unknown) > 0 {
			s.recordPendingMiss(agentID, unknown)
		}
		return
	}

	// Strong match. A pending miss from the same agent is the other half of a
	// reformulation; without one there is nothing to learn.
	pend, ok := s.peekPendingMiss(agentID)
	if !ok {
		return
	}

	pendSet := make(map[string]bool, len(pend.Terms))
	for _, t := range pend.Terms {
		pendSet[t] = true
	}

	// Delta: strong-query terms the weak query did not use, that the store
	// knows (df > 0 under the same fold the scorer applies). These are the
	// working words the caller just found.
	var delta []string
	for _, t := range toks {
		if pendSet[t] {
			continue
		}
		stemmed := tokenizeStem(t, doStem)
		if len(stemmed) == 0 || p.df[stemmed[0]] == 0 {
			continue
		}
		delta = append(delta, t)
	}
	if len(delta) == 0 {
		s.clearPendingMiss(agentID)
		return
	}

	promoted := false
	noGate := s.cfg.UsageAliasAffinityMin < 0
	if noGate {
		// Affinity gate disabled (measured mode for the synonym class): every
		// unknown word pairs with the most distinctive working word of the
		// episode — highest idf, alphabetical tie-break so the same usage
		// pattern always promotes the same alias. The pollution profile of
		// this mode is part of the measurement, not an assumption.
		bestIDF, bestWord := -1.0, ""
		for _, v := range delta {
			stemmed := tokenizeStem(v, s.cfg.StemKeywords)
			if len(stemmed) == 0 {
				continue
			}
			d := float64(p.df[stemmed[0]])
			if d == 0 {
				continue
			}
			if idf := math.Log((float64(len(p.facts)) + 1) / (d + 1)); idf > bestIDF || (idf == bestIDF && (bestWord == "" || v < bestWord)) {
				bestIDF = idf
				bestWord = v
			}
		}
		if bestWord == "" {
			s.clearPendingMiss(agentID)
			return
		}
		for _, u := range pend.Terms {
			count := s.recordPair(agentID, u, bestWord)
			if count >= usageAliasThreshold {
				if _, err := s.promoteUsageAlias(ctx, agentID, u, bestWord); err == nil {
					s.clearPair(agentID, u, bestWord)
					promoted = true
				}
			}
		}
		s.clearPendingMiss(agentID)
		_ = promoted
		return
	}
	for _, u := range pend.Terms {
		// Affinity gate: the store promotes a bridge between two words only
		// when they share at least the configured prefix — the
		// morphology/typo/near-form class, where co-occurrence evidence is
		// decisive. Pure synonyms carry no lexical signal the store can
		// count: guessing them from "these two words appeared in the same
		// sentence pair" is how "who = payments" gets promoted and every
		// later who-query inherits a term it never asked for. That class
		// belongs to the agent, via memory_alias — unless the affinity gate
		// is deliberately disabled, which is the measured synonym-class mode.
		bestV, bestAff := "", 0
		for _, v := range delta {
			if aff := commonPrefixLen(u, v); aff > bestAff && aff >= s.cfg.UsageAliasAffinityMin {
				bestAff = aff
				bestV = v
			}
		}
		if bestV == "" {
			continue
		}
		count := s.recordPair(agentID, u, bestV)
		if count >= usageAliasThreshold {
			if _, err := s.promoteUsageAlias(ctx, agentID, u, bestV); err == nil {
				s.clearPair(agentID, u, bestV)
				promoted = true
			}
		}
	}
	s.clearPendingMiss(agentID)
	_ = promoted // promotion is observable through the store; nothing to log here
}

// commonPrefixLen returns the length of the longest common leading run of
// characters of two lowercased words.
func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// recordPendingMiss replaces the agent's pending miss with the latest one.
func (s *Store) recordPendingMiss(agentID string, terms []string) {
	pm := pendingMiss{Terms: terms, At: s.now().UnixNano()}
	data, err := json.Marshal(pm)
	if err != nil {
		return
	}
	_ = s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketReformPending)
		if err != nil {
			return err
		}
		return b.Put([]byte(agentID), data)
	})
}

// peekPendingMiss returns the agent's pending miss if one exists and has not
// expired. Expired pendings are dropped on sight.
func (s *Store) peekPendingMiss(agentID string) (pendingMiss, bool) {
	var pm pendingMiss
	found := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketReformPending)
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(agentID))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &pm); err != nil {
			return nil
		}
		found = true
		return nil
	})
	if !found {
		return pendingMiss{}, false
	}
	// TTL on the store's clock, never on wall time: the store's now is the
	// only clock (newFact's contract), and a caller-controlled clock — a test
	// freeze, a replay — must not make every pending miss look 8 months old.
	if s.now().Sub(time.Unix(0, pm.At)) > pendingMissTTL {
		s.clearPendingMiss(agentID)
		return pendingMiss{}, false
	}
	return pm, true
}

// clearPendingMiss drops the agent's pending miss.
func (s *Store) clearPendingMiss(agentID string) {
	_ = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketReformPending)
		if b == nil {
			return nil
		}
		return b.Delete([]byte(agentID))
	})
}

// pairKey orders the two terms so both observation directions accumulate
// under one key: A paired with B and B paired with A are the same vocabulary
// relation.
func pairKey(agentID, u, v string) string {
	a, b := u, v
	if a > b {
		a, b = b, a
	}
	return agentID + "\x00" + a + "\x00" + b
}

// recordPair increments the evidence count for one pair and returns it.
func (s *Store) recordPair(agentID, u, v string) int {
	key := pairKey(agentID, u, v)
	count := 0
	_ = s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketReformPairs)
		if err != nil {
			return err
		}
		var p reformPair
		if raw := b.Get([]byte(key)); raw != nil {
			if json.Unmarshal(raw, &p) != nil {
				p = reformPair{}
			}
		}
		p.Count++
		p.LastAt = s.now().UnixNano()
		count = p.Count
		data, err := json.Marshal(p)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), data)
	})
	return count
}

// clearPair drops the evidence after a successful promotion.
func (s *Store) clearPair(agentID, u, v string) {
	_ = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketReformPairs)
		if b == nil {
			return nil
		}
		return b.Delete([]byte(pairKey(agentID, u, v)))
	})
}

// promoteUsageAlias writes the alias fact the pair earned, marked as
// store-promoted so it is distinguishable from agent curation forever.
func (s *Store) promoteUsageAlias(ctx context.Context, agentID, u, v string) (Fact, error) {
	return s.putReturningFactKind(ctx, agentID, aliasPrefix+" "+u+" = "+v, KindAlias, AliasSourceUsage)
}

// pruneReformPairs expires pairs whose evidence window closed without
// reaching the promotion threshold. Runs once at Open.
func (s *Store) pruneReformPairs() {
	cutoff := s.now().Add(-pairMaxAge).UnixNano()
	_ = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketReformPairs)
		if b == nil {
			return nil
		}
		var stale [][]byte
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var p reformPair
			if json.Unmarshal(v, &p) != nil {
				stale = append(stale, append([]byte{}, k...))
				continue
			}
			if p.LastAt < cutoff {
				stale = append(stale, append([]byte{}, k...))
			}
		}
		for _, k := range stale {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// countUsageAliases returns how many live alias facts the store promoted
// itself. Measurement surface for the pre-registered gate, not part of the
// product API.
func (s *Store) countUsageAliases(agentID string) int {
	n := 0
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFacts)
		if b == nil {
			return nil
		}
		ab := b.Bucket([]byte(agentID))
		if ab == nil {
			return nil
		}
		return ab.ForEach(func(_, v []byte) error {
			f, err := unmarshalFactLite(v)
			if err != nil {
				return nil
			}
			if f.Kind == KindAlias && !f.IsSuperseded() && f.AliasSource == AliasSourceUsage {
				n++
			}
			return nil
		})
	})
	return n
}

// usageAliasTerms returns the term pairs behind every live usage alias, for
// verification and audit output.
func (s *Store) usageAliasTerms(agentID string) []string {
	var out []string
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFacts)
		if b == nil {
			return nil
		}
		ab := b.Bucket([]byte(agentID))
		if ab == nil {
			return nil
		}
		return ab.ForEach(func(_, v []byte) error {
			f, err := unmarshalFactLite(v)
			if err != nil {
				return nil
			}
			if f.Kind == KindAlias && !f.IsSuperseded() && f.AliasSource == AliasSourceUsage {
				out = append(out, strings.TrimPrefix(f.Text, aliasPrefix))
			}
			return nil
		})
	})
	sort.Strings(out)
	return out
}
