package memory

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// The candidate-set index.
//
// Until now every recall loaded every fact the agent owned and re-tokenised
// all of them, per query. Measured on a synthetic corpus: 5.6 ms at 600 facts,
// 24 ms at 3000, 78 ms at 10 000, 236 ms at 30 000 — 50x the corpus for 42x
// the time, with the load alone accounting for 53-63% of it. That is a linear
// retrieval path wearing a hybrid ranker, and no amount of scoring cleverness
// fixes an exponent.
//
// What makes the linear scan hard to remove is not the keyword signal — it is
// the other two. The fusion gives every live fact a recency rank over the
// WHOLE corpus, so a fact that matches nothing can still surface on freshness
// alone, and its score depends on how many facts are newer than it. Any
// candidate set that simply drops non-matching facts silently re-specifies the
// ranking.
//
// The way out is that the recency signal needs no text. This index keeps two
// structures:
//
//	terms     an inverted index, term -> the facts containing it, plus each
//	          term's document frequency. Postings are one key per (term, fact)
//	          rather than a packed list, so a write inserts a ~30-byte key
//	          instead of rewriting a posting list that, for a ubiquitous term,
//	          is the size of the corpus.
//	recency   every live fact as (createdAt, id, flags) and nothing else — no
//	          text, no JSON. Scanning it yields the exact global recency order
//	          and the exact live-fact count that idf needs.
//
// With those, a recall computes the true fused score of every fact while only
// loading the text of the ones a query term actually reached. The recency-only
// tail is scored exactly, from the spine, without ever being deserialised.
//
// Postings are kept sorted by fact ID (bbolt's key order, and ULIDs are
// monotonic) and each term carries its own df. That is the WAND/MaxScore
// precondition: an upper bound per term plus ordered postings is what lets a
// later pruning layer skip documents that cannot reach the top-k, and it makes
// that layer an addition rather than a rewrite. This file does not prune yet —
// see indexCandidates for where the essential/non-essential split lands.
var (
	bucketIdxTerms   = []byte("idx_terms")
	bucketIdxRecency = []byte("idx_recency")
	bucketIdxMeta    = []byte("idx_meta")
)

// indexVersion is bumped whenever the on-disk layout or the tokenisation
// contract changes. A mismatch rebuilds rather than misreads.
const indexVersion = 2

// postingSep separates a term from a fact ID inside the terms bucket. NUL
// sorts below every byte a token can contain, so a prefix scan of
// "<term>\x00" stops cleanly at the next term and the df counter — stored at
// the bare "<term>" key — sorts immediately before its own postings.
const postingSep = 0x00

// idxFlag bits on a recency entry. Both are properties the ranking needs
// before it has any text: a superseded fact leaves the corpus entirely, and an
// alias fact is vocabulary rather than content and never enters the ranking.
const (
	idxFlagSuperseded = 1 << 0
	idxFlagAlias      = 1 << 1
)

// indexState is the per-agent stamp that decides whether the index can be
// trusted. Facts is the fact count at the last maintained write: a mismatch
// against the live bucket means some writer bypassed maintenance, and the
// index rebuilds instead of answering from a stale picture. Stemmed records
// the fold the postings were built with, because StemKeywords changes what a
// token is and an index built under one fold cannot answer under the other.
type indexState struct {
	Version int  `json:"version"`
	Facts   int  `json:"facts"`
	Stemmed bool `json:"stemmed"`
	// Writes counts every maintained mutation, not just the ones that change
	// the fact count. A revision leaves Facts untouched while moving a fact's
	// flags and possibly its timestamp, so a cache keyed on Facts alone would
	// serve a spine that no longer matches the store. This is the key the
	// in-memory spine cache is validated against, and it only ever goes up.
	Writes uint64 `json:"writes"`
}

// recencyKey packs a fact's position in time ahead of its ID. Nanoseconds are
// big-endian so bbolt's byte order is chronological order, and the ID breaks
// ties deterministically — the same total order rankBefore uses.
func recencyKey(createdAt time.Time, factID string) []byte {
	k := make([]byte, 8+len(factID))
	binary.BigEndian.PutUint64(k[:8], uint64(createdAt.UnixNano()))
	copy(k[8:], factID)
	return k
}

func postingKey(term, factID string) []byte {
	k := make([]byte, 0, len(term)+1+len(factID))
	k = append(k, term...)
	k = append(k, postingSep)
	k = append(k, factID...)
	return k
}

// recencyValue packs everything the ranking needs about a fact that is not
// its text: the two corpus-membership flags, and the fact's token count.
//
// The length is what turns the keyword signal into pure arithmetic. The
// scorer divides a fact's tf-idf sum by its length, so with the length here
// and the term frequency on the posting, a keyword score is computable
// without reading, decoding or re-tokenising a single fact. That is what
// removes the last O(N) text pass from the query path — and it is what makes
// a ubiquitous term (df = N) cost N multiplications instead of N JSON
// decodes.
func recencyValue(f Fact, docLen int) []byte {
	buf := make([]byte, 1, 1+binary.MaxVarintLen64)
	buf[0] = indexFlags(f)
	return binary.AppendUvarint(buf, uint64(docLen))
}

func decodeRecencyValue(v []byte) (flags byte, docLen int) {
	if len(v) == 0 {
		return 0, 0
	}
	flags = v[0]
	if len(v) > 1 {
		n, read := binary.Uvarint(v[1:])
		if read > 0 {
			docLen = int(n)
		}
	}
	return flags, docLen
}

func indexFlags(f Fact) byte {
	var b byte
	if f.IsSuperseded() {
		b |= idxFlagSuperseded
	}
	if f.Kind == KindAlias {
		b |= idxFlagAlias
	}
	return b
}

// indexTerms is the term-frequency map a fact contributes to the inverted
// index, plus its token count. Superseded and alias facts contribute nothing:
// neither is in the ranking corpus, so neither may appear in a posting list or
// a document frequency.
func indexTerms(f Fact, doStem bool) (map[string]int, int) {
	if f.IsSuperseded() || f.Kind == KindAlias {
		return nil, 0
	}
	toks := tokenizeStem(f.Text, doStem)
	if len(toks) == 0 {
		return nil, 0
	}
	tf := make(map[string]int, len(toks))
	for _, t := range toks {
		tf[t]++
	}
	return tf, len(toks)
}

// --- maintenance ------------------------------------------------------------

// idxAddFact writes one fact's index entries inside the caller's transaction.
// Every write path that creates a fact calls it; the transaction is the
// caller's so the index can never be durable while the fact is not, or
// the reverse.
func idxAddFact(tx *bolt.Tx, f Fact, doStem bool) error {
	rb, err := idxBucket(tx, bucketIdxRecency, f.AgentID)
	if err != nil {
		return err
	}
	terms, docLen := indexTerms(f, doStem)
	if err := rb.Put(recencyKey(f.CreatedAt, f.ID), recencyValue(f, docLen)); err != nil {
		return err
	}
	if len(terms) == 0 {
		return nil
	}
	tb, err := idxBucket(tx, bucketIdxTerms, f.AgentID)
	if err != nil {
		return err
	}
	for term, count := range terms {
		if err := tb.Put(postingKey(term, f.ID), binary.AppendUvarint(nil, uint64(count))); err != nil {
			return err
		}
		if err := idxBumpDF(tb, term, +1); err != nil {
			return err
		}
	}
	return nil
}

// idxRemoveFact erases one fact's index entries. It takes the fact as it was
// last indexed, not as the caller wishes it were: the posting keys to delete
// are derived from the stored text, so a caller that hands over a rewritten
// fact would strand postings under the old terms.
func idxRemoveFact(tx *bolt.Tx, f Fact, doStem bool) error {
	if rb := idxBucketRO(tx, bucketIdxRecency, f.AgentID); rb != nil {
		if err := rb.Delete(recencyKey(f.CreatedAt, f.ID)); err != nil {
			return err
		}
	}
	terms, _ := indexTerms(f, doStem)
	if len(terms) == 0 {
		return nil
	}
	tb := idxBucketRO(tx, bucketIdxTerms, f.AgentID)
	if tb == nil {
		return nil
	}
	for term := range terms {
		key := postingKey(term, f.ID)
		if tb.Get(key) == nil {
			continue // never indexed under this term; df must not go negative
		}
		if err := tb.Delete(key); err != nil {
			return err
		}
		if err := idxBumpDF(tb, term, -1); err != nil {
			return err
		}
	}
	return nil
}

// idxBumpDF moves a term's document frequency and drops the counter when it
// reaches zero, so a vocabulary that churns does not leave a growing tail of
// zero-df terms behind.
func idxBumpDF(tb *bolt.Bucket, term string, delta int) error {
	key := []byte(term)
	n := 0
	if raw := tb.Get(key); len(raw) == 8 {
		n = int(binary.BigEndian.Uint64(raw))
	}
	n += delta
	if n <= 0 {
		return tb.Delete(key)
	}
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(n))
	return tb.Put(key, buf)
}

func idxBucket(tx *bolt.Tx, root []byte, agentID string) (*bolt.Bucket, error) {
	parent, err := tx.CreateBucketIfNotExists(root)
	if err != nil {
		return nil, err
	}
	return parent.CreateBucketIfNotExists([]byte(agentID))
}

func idxBucketRO(tx *bolt.Tx, root []byte, agentID string) *bolt.Bucket {
	parent := tx.Bucket(root)
	if parent == nil {
		return nil
	}
	return parent.Bucket([]byte(agentID))
}

// idxBumpCount moves the agent's recorded fact count by delta. The count is
// the self-heal signal: a process compares it against the real bucket once,
// the first time it recalls for that agent, and rebuilds on disagreement — so
// a write path that forgets to maintain the index degrades to "slower until
// the next process" rather than "wrong answers".
//
// It is a delta and not a recount for a reason that cost a measurement to
// find. The first version called Bucket.Stats().KeyN on every write; Stats
// walks every page of the bucket, so maintaining the index made Put O(N).
// The scale run showed it plainly — p50 Put climbing 2.0 ms → 3.0 ms as the
// corpus grew, against a flat 1.05 ms before the index existed — which is a
// linear write path bolted onto the sub-linear read path it was there to
// create.
func idxBumpCount(tx *bolt.Tx, agentID string, delta int, doStem bool) error {
	mb, err := tx.CreateBucketIfNotExists(bucketIdxMeta)
	if err != nil {
		return err
	}
	st := indexState{Version: indexVersion, Stemmed: doStem}
	if raw := mb.Get([]byte(agentID)); raw != nil {
		var prev indexState
		if json.Unmarshal(raw, &prev) == nil && prev.Version == indexVersion && prev.Stemmed == doStem {
			st.Facts = prev.Facts
			st.Writes = prev.Writes
		}
	}
	st.Facts += delta
	st.Writes++
	if st.Facts < 0 {
		st.Facts = 0
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return mb.Put([]byte(agentID), data)
}

// idxSetCount writes the count outright. Only the rebuild uses it, which is
// the one place a full recount is already being paid for.
func idxSetCount(tx *bolt.Tx, agentID string, doStem bool) error {
	n := 0
	if fb := tx.Bucket(bucketFacts); fb != nil {
		if b := fb.Bucket([]byte(agentID)); b != nil {
			n = b.Stats().KeyN
		}
	}
	mb, err := tx.CreateBucketIfNotExists(bucketIdxMeta)
	if err != nil {
		return err
	}
	// A rebuild replaces the spine wholesale, so the write counter has to move
	// past whatever a cache is holding — otherwise a rebuild that happens to
	// land on the same fact count would leave a stale spine looking fresh.
	var writes uint64
	if raw := mb.Get([]byte(agentID)); raw != nil {
		var prev indexState
		if json.Unmarshal(raw, &prev) == nil {
			writes = prev.Writes
		}
	}
	data, err := json.Marshal(indexState{
		Version: indexVersion, Facts: n, Stemmed: doStem, Writes: writes + 1,
	})
	if err != nil {
		return err
	}
	return mb.Put([]byte(agentID), data)
}

func idxReadState(tx *bolt.Tx, agentID string) (indexState, bool) {
	mb := tx.Bucket(bucketIdxMeta)
	if mb == nil {
		return indexState{}, false
	}
	raw := mb.Get([]byte(agentID))
	if raw == nil {
		return indexState{}, false
	}
	var st indexState
	if err := json.Unmarshal(raw, &st); err != nil {
		return indexState{}, false
	}
	return st, true
}

// idxRebuild throws away an agent's index and derives it again from the facts
// bucket. It is the only O(N) path left, and it runs when the index is absent,
// stale, built under a different tokenisation fold, or written behind the
// maintenance hooks' back.
func (s *Store) idxRebuild(agentID string) error {
	doStem := s.cfg.StemKeywords
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, root := range [][]byte{bucketIdxTerms, bucketIdxRecency} {
			parent, err := tx.CreateBucketIfNotExists(root)
			if err != nil {
				return err
			}
			if parent.Bucket([]byte(agentID)) != nil {
				if err := parent.DeleteBucket([]byte(agentID)); err != nil {
					return err
				}
			}
		}
		fb := tx.Bucket(bucketFacts)
		if fb == nil {
			return idxSetCount(tx, agentID, doStem)
		}
		b := fb.Bucket([]byte(agentID))
		if b == nil {
			return idxSetCount(tx, agentID, doStem)
		}
		if err := b.ForEach(func(_, v []byte) error {
			f, err := unmarshalFactLite(v)
			if err != nil {
				return nil // corrupt entries are skipped by every other reader too
			}
			return idxAddFact(tx, f, doStem)
		}); err != nil {
			return err
		}
		return idxSetCount(tx, agentID, doStem)
	})
}

// --- query ------------------------------------------------------------------

// idxSpineEntry is one live fact as the ranking sees it before any text is
// read: identity, age and the two flags that decide whether it is in the
// corpus at all.
type idxSpineEntry struct {
	id      string
	created time.Time
	flags   byte
	docLen  int
	// tie is the entry's position in the OLDEST-first order, which is exactly
	// the ranking's tie-break: equal scores go to the older fact, and to the
	// lower fact ID when the timestamps match too.
	//
	// It is stored as an int so the ranking comparators never touch a map. At
	// 30 000 facts the final sort makes on the order of 450 000 comparisons,
	// and a comparator that resolved ties through a map did two lookups on
	// each of them — about a million hash probes per query, which is how the
	// candidate path ended up with a p99 worse than the scan it replaced
	// while its p50 was fourteen times better.
	tie int
}

// idxSpineAsc returns every indexed fact in bbolt's own key order — oldest
// first, and within one timestamp, fact ID ascending. The caller reverses it
// group-wise into the ranking's order; see idxSpineOrdered for why reversing
// entry-by-entry would be wrong.
func idxSpineAsc(tx *bolt.Tx, agentID string) []idxSpineEntry {
	rb := idxBucketRO(tx, bucketIdxRecency, agentID)
	if rb == nil {
		return nil
	}
	out := make([]idxSpineEntry, 0, rb.Stats().KeyN)
	c := rb.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if len(k) < 9 {
			continue
		}
		flags, docLen := decodeRecencyValue(v)
		out = append(out, idxSpineEntry{
			id:      string(k[8:]),
			created: time.Unix(0, int64(binary.BigEndian.Uint64(k[:8]))).UTC(),
			flags:   flags,
			docLen:  docLen,
		})
	}
	return out
}

// idxDF reads one term's document frequency. Zero means the store has never
// seen the word — which is exactly the signal the weak-match trigger reads.
func idxDF(tb *bolt.Bucket, term string) int {
	if tb == nil {
		return 0
	}
	raw := tb.Get([]byte(term))
	if len(raw) != 8 {
		return 0
	}
	return int(binary.BigEndian.Uint64(raw))
}

// idxPostings appends the IDs of every fact containing term to out.
func idxPostings(tb *bolt.Bucket, term string, out map[string]bool) {
	idxWalkPostings(tb, term, func(id string, _ int) { out[id] = true })
}

// idxWalkPostings visits one term's posting list in fact-ID order, handing the
// caller each fact and its term frequency. The ordering is bbolt's own and is
// what a later WAND/MaxScore layer skips along with Seek.
func idxWalkPostings(tb *bolt.Bucket, term string, fn func(factID string, tf int)) {
	if tb == nil {
		return
	}
	prefix := append([]byte(term), postingSep)
	c := tb.Cursor()
	for k, v := c.Seek(prefix); k != nil; k, v = c.Next() {
		if len(k) <= len(prefix) || string(k[:len(prefix)]) != string(prefix) {
			break
		}
		tf := 1
		if len(v) > 0 {
			if n, read := binary.Uvarint(v); read > 0 {
				tf = int(n)
			}
		}
		fn(string(k[len(prefix):]), tf)
	}
}

// indexCandidates is the candidate set for one query: every fact containing at
// least one query term. Facts outside it have a keyword score of exactly zero
// — keywordScoreDetailed records no entry for them and the fusion adds no
// keyword contribution — so their fused score is fully determined by the
// spine, and none of their text has to be read.
//
// This is where a WAND/MaxScore layer goes when a ubiquitous term makes the
// candidate set the whole corpus again: the per-term df is already here, so
// the maximum contribution of each term is computable, terms split into
// essential and non-essential, and postings that cannot lift a document over
// the running top-k threshold get skipped. The postings are stored sorted by
// fact ID precisely so that skipping is a cursor Seek rather than a redesign.
func indexCandidates(tx *bolt.Tx, agentID string, terms []string) (map[string]bool, map[string]int) {
	tb := idxBucketRO(tx, bucketIdxTerms, agentID)
	cands := make(map[string]bool)
	dfs := make(map[string]int, len(terms))
	for _, t := range terms {
		dfs[t] = idxDF(tb, t)
		idxPostings(tb, t, cands)
	}
	return cands, dfs
}

// idxUsable reports whether the agent's index can answer right now.
//
// Two strengths of check, because they cost differently. Version and
// tokenisation fold are two field comparisons and run every time. The fact
// count is a Bucket.Stats() walk over every page — the expensive check — and
// runs once per agent per process: it exists to catch a writer that bypassed
// maintenance, which is a code-level mistake, not something that appears
// between two recalls of a running process. After the first verification the
// maintained counter is trusted, which is what keeps recall off the O(N) path
// it was built to escape.
func (s *Store) idxUsable(agentID string) (bool, error) {
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error {
		st, found := idxReadState(tx, agentID)
		if !found || st.Version != indexVersion || st.Stemmed != s.cfg.StemKeywords {
			return nil
		}
		if s.idxCounted(agentID) {
			ok = true
			return nil
		}
		n := 0
		if fb := tx.Bucket(bucketFacts); fb != nil {
			if b := fb.Bucket([]byte(agentID)); b != nil {
				n = b.Stats().KeyN
			}
		}
		ok = st.Facts == n
		return nil
	})
	if ok {
		s.idxMarkCounted(agentID)
	}
	return ok, err
}

// idxCounted / idxMarkCounted remember which agents this process has already
// verified the hard way.
func (s *Store) idxCounted(agentID string) bool {
	s.idxVerifiedMu.Lock()
	defer s.idxVerifiedMu.Unlock()
	return s.idxVerified[agentID]
}

func (s *Store) idxMarkCounted(agentID string) {
	s.idxVerifiedMu.Lock()
	defer s.idxVerifiedMu.Unlock()
	if s.idxVerified == nil {
		s.idxVerified = make(map[string]bool)
	}
	s.idxVerified[agentID] = true
}

// idxEnsure makes the index usable, rebuilding it if it is not. A read-only
// store cannot rebuild, so it reports unusable and the caller falls back to
// the scan — a degraded open must stay correct, never refuse to answer.
func (s *Store) idxEnsure(agentID string) bool {
	ok, err := s.idxUsable(agentID)
	if err != nil {
		return false
	}
	if ok {
		return true
	}
	if s.readOnly {
		return false
	}
	s.idxForgetCounted(agentID)
	if err := s.idxRebuild(agentID); err != nil {
		return false
	}
	ok, err = s.idxUsable(agentID)
	return err == nil && ok
}

// idxLoadFacts reads the lite form of exactly the requested IDs.
func (s *Store) idxLoadFacts(tx *bolt.Tx, agentID string, ids map[string]bool) (map[string]Fact, error) {
	out := make(map[string]Fact, len(ids))
	fb := tx.Bucket(bucketFacts)
	if fb == nil {
		return out, nil
	}
	b := fb.Bucket([]byte(agentID))
	if b == nil {
		return out, nil
	}
	for id := range ids {
		raw := b.Get([]byte(id))
		if raw == nil {
			continue
		}
		f, err := unmarshalFactLite(raw)
		if err != nil {
			return nil, fmt.Errorf("index load %s: %w", id, err)
		}
		out[id] = f
	}
	return out, nil
}

// errIndexUnusable says the candidate-set path cannot answer for this agent —
// no index, a stale one on a read-only store, or a tokenisation fold it was
// not built under. It is a routing signal, never an error the caller sees:
// recall falls back to the scan, which is always correct and only slower.
var errIndexUnusable = errors.New("memory: candidate index unusable")

// idxForgetCounted drops the process-local verification so the next usability
// check re-counts. A rebuild is exactly when the count must be re-established
// from the facts rather than from the record it just replaced.
func (s *Store) idxForgetCounted(agentID string) {
	s.idxVerifiedMu.Lock()
	defer s.idxVerifiedMu.Unlock()
	delete(s.idxVerified, agentID)
}
