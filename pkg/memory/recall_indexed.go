package memory

import (
	"context"
	"math"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// The candidate-set recall path.
//
// It computes the same fused ranking as the scan path — same scores, same
// order, byte-for-byte — while reading the text of only the facts a query term
// actually reached. What makes that possible is that two of the three signals
// need no text at all:
//
//	recency   a fact's rank is its position in the global newest-first order,
//	          which the index's recency spine yields without deserialising
//	          anything.
//	keyword   keywordScoreDetailed records no score for a fact that contains
//	          no query term, and the fusion adds no keyword contribution for a
//	          fact with no score. Those facts are exactly the ones outside the
//	          posting lists, so their absence from the candidate set costs the
//	          ranking nothing.
//	vector    the vector store returns IDs; the handful it names join the
//	          candidate set.
//
// So every live fact still gets its true fused score, and the ones nobody
// asked about get it for the price of a float multiply instead of a JSON
// decode plus a re-tokenisation.
//
// What stays O(N) — deliberately, and measured rather than hidden:
//   - the spine scan and the final sort, both over id+timestamp pairs with no
//     text: cheap constants, and the sort is the next thing to go (the
//     recency-only tail arrives already ordered, so a merge would replace it).
//   - the tombstone load, which is O(revisions) rather than O(facts) and only
//     exists to answer "what did this replace" on a receipt.
//   - the weak-match neighbourhood, which walks the seed terms' posting lists
//     instead of the corpus, and only when the trigger fires.

// runRecallPipelineIndexed is the candidate-set twin of runRecallPipeline.
// It returns (nil, errIndexUnusable) when the index cannot answer, and the
// caller falls back to the scan — a store that has never been written through
// a maintaining path, or one opened read-only before its first rebuild, must
// still answer correctly.
func (s *Store) runRecallPipelineIndexed(ctx context.Context, agentID, query string, topK int) (*recallPipeline, error) {
	start := time.Now()
	doStem := s.cfg.StemKeywords

	// The spine's three populations, and the id -> position map the scorer
	// needs. All four are pure functions of the index state, so they are cached
	// together: rebuilding them meant a 30 000-entry map insert and a
	// 30 000-element filter pass on every query, for data that only changes
	// when something writes.
	part, ok := s.idxPartition(agentID)
	if !ok {
		return nil, errIndexUnusable
	}
	live, spineIdx := part.live, part.index
	aliasIDs, tombIDs := part.aliasIDs, part.tombIDs
	if len(live) == 0 {
		if s.cfg.OnRecall != nil {
			s.cfg.OnRecall(agentID, query, 0, time.Since(start))
		}
		return nil, nil
	}

	aliases, err := s.idxFetch(agentID, aliasIDs)
	if err != nil {
		return nil, err
	}
	aliasMap := parseAliasFacts(aliases)
	effectiveQuery := expandQuery(query, aliasMap, doStem)

	// Vector search names its own candidates; they join the set so a fact the
	// embedder likes is scored and returnable even when it shares no term with
	// the query.
	vectorRank := make(map[string]int, topK*2)
	vecResults, _ := s.vectorSearch(ctx, agentID, effectiveQuery, topK*2)
	sort.SliceStable(vecResults, func(i, j int) bool {
		if vecResults[i].Similarity != vecResults[j].Similarity {
			return vecResults[i].Similarity > vecResults[j].Similarity
		}
		return vecResults[i].ID < vecResults[j].ID
	})
	for i, r := range vecResults {
		vectorRank[r.ID] = i + 1
	}

	queryTerms := tokenizeStem(effectiveQuery, doStem)
	uniqTerms := dedupTokens(queryTerms)

	// Texts are read lazily and only for what a caller will actually see: the
	// head of the ranking, plus whatever the weak-match diagnostic asks about.
	// Nothing here loads a fact in order to score it.
	loaded := make(map[string]Fact, topK*4)
	tfByID := make(map[string]map[string]int, topK*4)

	// --- Signal 2: keyword relevance, computed from the index alone ---
	//
	// No text is read here. The posting carries the term frequency and the
	// spine carries the fact's token count, so the scorer's own arithmetic —
	// sum of tf*idf over the query's terms, divided by length plus one — runs
	// on integers pulled straight out of the index. That is what makes a
	// ubiquitous term affordable: a term in all 30 000 facts costs 30 000
	// multiplications instead of 30 000 JSON decodes and re-tokenisations.
	//
	// Query terms are counted with their multiplicity, because the scan path
	// loops over the query's tokens rather than its distinct terms: a query
	// that says a word twice weights it twice, and identity is identity.
	mult := make(map[string]int, len(uniqTerms))
	for _, t := range queryTerms {
		mult[t]++
	}
	n := float64(len(live))
	dfCache := make(map[string]int, len(uniqTerms)*2)
	// Accumulate into a slice indexed by spine position, not a map keyed by
	// fact ID. A ubiquitous term makes every fact a candidate, and at 30 000
	// facts the map version paid 30 000 inserts plus a second 30 000-entry map
	// for the normalised scores — on the tail query that was most of the
	// query. The slice is one allocation and a bounds-checked write, and the
	// id -> position map it needs is the one the partition cache already
	// holds.
	raw := make([]float64, len(live))
	if err := s.db.View(func(tx *bolt.Tx) error {
		tb := idxBucketRO(tx, bucketIdxTerms, agentID)
		for _, t := range uniqTerms {
			df := idxDF(tb, t)
			dfCache[t] = df
			if df == 0 {
				continue
			}
			w := float64(mult[t]) * math.Log((n+1)/(float64(df)+1))
			if w == 0 {
				// idf of a term in every fact is exactly zero: it cannot
				// change any score, so it cannot create a candidate either.
				// Walking its posting list would be the whole corpus for
				// nothing.
				//
				// Note how narrow this is: five planted facts that happen not
				// to contain the word are enough to make df < n, idf a
				// millionth instead of zero, and the whole corpus a candidate
				// set again. Skipping a term whose idf is merely negligible
				// would be sound arithmetic and unsound ranking — the fusion
				// reads keyword RANKS, so dropping a term changes which facts
				// have a keyword rank at all and renumbers everything below.
				// That is a re-specification, not an optimisation.
				continue
			}
			idxWalkPostings(tb, t, func(id string, tf int) {
				if i, isLive := spineIdx[id]; isLive {
					raw[i] += float64(tf) * w
				}
			})
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// The scan path's rankBefore — score descending, then oldest first, then
	// fact ID — reduced to two integer comparisons. The spine's tie rank is
	// precisely "oldest first, then ID ascending", so no comparison here
	// touches a timestamp, a string or a map.
	type entry struct {
		id    string
		score float64
		tie   int
	}
	less := func(a, b entry) bool {
		if a.score != b.score {
			return a.score > b.score
		}
		return a.tie < b.tie
	}

	kwSorted := make([]entry, 0, 64)
	for i := range live {
		if raw[i] == 0 {
			continue
		}
		if sc := raw[i] / float64(live[i].docLen+1); sc > 0 {
			kwSorted = append(kwSorted, entry{live[i].id, sc, live[i].tie})
		}
	}
	sort.Slice(kwSorted, func(i, j int) bool { return less(kwSorted[i], kwSorted[j]) })
	kwRank := make(map[string]int, len(kwSorted))
	for i, e := range kwSorted {
		kwRank[e.id] = i + 1
	}

	nowT := s.now()

	// --- RRF fusion, over every live fact ---
	const k = 60.0
	w := s.cfg.SignalWeights
	if w == nil {
		d := DefaultSignalWeights()
		w = &d
	}
	fused := make([]entry, len(live))
	for i := range live {
		id := live[i].id
		rrf := w.Recency / (k + float64(i+1))
		if r, hit := vectorRank[id]; hit {
			rrf += w.Vector / (k + float64(r))
		}
		if r, hit := kwRank[id]; hit {
			rrf += w.Keyword / (k + float64(r))
		}
		fused[i] = entry{id, rrf, live[i].tie}
	}
	sort.Slice(fused, func(i, j int) bool { return less(fused[i], fused[j]) })
	allScored := make([]scored, len(fused))
	for i, e := range fused {
		allScored[i] = scored{e.id, e.score}
	}

	if s.debugRanking != nil {
		snapshot := make([]scored, len(allScored))
		copy(snapshot, allScored)
		s.debugRanking(query, snapshot)
	}

	if s.cfg.MinRelevance > 0 && len(allScored) > 0 {
		floor := s.cfg.MinRelevance * allScored[0].score
		cut := len(allScored)
		for i, sc := range allScored {
			if sc.score < floor {
				cut = i
				break
			}
		}
		allScored = allScored[:cut]
	}

	// The head of the ranking has to be readable: RecallDetailed dedups by
	// text as it walks it, so "enough" is not topK entries but topK distinct
	// texts. Widen until that holds or the ranking runs out.
	if err := s.idxLoadHead(agentID, allScored, topK, loaded); err != nil {
		return nil, err
	}

	// Tombstones, for the KG filter and for receipt lineage. O(revisions),
	// not O(facts) — and skipped entirely when nothing will read it.
	var supersededTexts map[string]bool
	var retiredBy map[string][]string
	s.mu.RLock()
	wantsTexts := s.graph != nil
	s.mu.RUnlock()
	if len(tombIDs) > 0 {
		tombs, terr := s.idxFetch(agentID, tombIDs)
		if terr != nil {
			return nil, terr
		}
		supersededTexts = make(map[string]bool, len(tombs))
		retiredBy = make(map[string][]string)
		for _, f := range tombs {
			if wantsTexts {
				supersededTexts[f.Text] = true
			}
			if f.SupersededBy != SupersededByAgent {
				retiredBy[f.SupersededBy] = append(retiredBy[f.SupersededBy], f.ID)
			}
		}
	}

	// recRank is receipt data, read per returned fact and never by the
	// ranking — the fusion above already had each fact's position as its loop
	// index. Building it for all 30 000 would be 30 000 map inserts per query
	// to answer eight questions.
	recRank := make(map[string]int, len(loaded))
	for id := range loaded {
		if i, hit := spineIdx[id]; hit {
			recRank[id] = i + 1
		}
	}

	factByID := make(map[string]*Fact, len(loaded))
	factsOut := make([]Fact, 0, len(loaded))
	for _, f := range loaded {
		factsOut = append(factsOut, f)
	}
	for i := range factsOut {
		factByID[factsOut[i].ID] = &factsOut[i]
	}

	// df for the usage-alias learning hook reads the raw query's terms as well as
	// the expanded one's: an unknown word is the whole signal, and it must be
	// looked up under the same fold the scorer used.
	dfOut := make(map[string]int, len(dfCache)+8)
	for t, v := range dfCache {
		dfOut[t] = v
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		tb := idxBucketRO(tx, bucketIdxTerms, agentID)
		for _, raw := range dedupTokens(tokenize(query)) {
			for _, t := range tokenizeStem(raw, doStem) {
				if _, have := dfOut[t]; !have {
					dfOut[t] = idxDF(tb, t)
				}
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	feedback := weakMatchFeedback(weakMatchInput{
		originalQuery:  query,
		effectiveQuery: effectiveQuery,
		doStem:         doStem,
		n:              len(live),
		df:             func(t string) int { return s.idxDFCached(agentID, t, dfOut) },
		tf: func(id string) map[string]int {
			if tf, hit := tfByID[id]; hit {
				return tf
			}
			f, hit := loaded[id]
			if !hit {
				return nil
			}
			tf := make(map[string]int)
			for _, t := range tokenizeStem(f.Text, doStem) {
				tf[t]++
			}
			tfByID[id] = tf
			return tf
		},
		seedDocs: func(seeds map[string]bool) []map[string]int {
			return s.idxSeedDocs(agentID, seeds, spineIdx, doStem)
		},
		ranked: allScored,
		topK:   topK,
	})

	return &recallPipeline{
		facts:           factsOut,
		factByID:        factByID,
		ranked:          allScored,
		vectorRank:      vectorRank,
		kwRank:          kwRank,
		recRank:         recRank,
		k:               k,
		nowT:            nowT,
		feedback:        feedback,
		df:              dfOut,
		supersededTexts: supersededTexts,
		retiredBy:       retiredBy,
	}, nil
}

// idxSpineOrdered returns the live+tombstoned spine in the exact order
// sortFactsByTime produces: CreatedAt descending, and for facts sharing a
// timestamp, fact ID ascending.
//
// The tie-break is not cosmetic. bbolt hands the spine back in key order,
// which is timestamp-then-ID ascending, so walking it backwards would order
// same-instant facts by DESCENDING id — the opposite of what the scan path
// does, and enough on its own to permute the ranking of every fact written
// inside one clock tick (Windows ticks at ~15.6 ms, so that is not a rare
// case). Walking forward and emitting whole timestamp groups from the back
// keeps both directions right in one pass.
func (s *Store) idxSpineOrdered(agentID string) ([]idxSpineEntry, bool) {
	if !s.idxEnsure(agentID) {
		return nil, false
	}
	// The spine is the same slice for every query until something writes, and
	// rebuilding it meant one bbolt cursor pass plus one string allocation per
	// fact — about 30 000 allocations a query at the top of the scale gate,
	// for data that had not changed. It is cached against the index's write
	// counter, which moves on every maintained mutation: a revision that
	// leaves the fact count alone still invalidates.
	var st indexState
	var found bool
	if err := s.db.View(func(tx *bolt.Tx) error {
		st, found = idxReadState(tx, agentID)
		return nil
	}); err != nil {
		return nil, false
	}
	if found {
		if cached, ok := s.spineCached(agentID, st); ok {
			return cached, true
		}
	}

	var asc []idxSpineEntry
	if err := s.db.View(func(tx *bolt.Tx) error {
		asc = idxSpineAsc(tx, agentID)
		return nil
	}); err != nil {
		return nil, false
	}
	for i := range asc {
		asc[i].tie = i // ascending order IS the tie-break order
	}
	out := make([]idxSpineEntry, 0, len(asc))
	for end := len(asc); end > 0; {
		start := end - 1
		for start > 0 && asc[start-1].created.Equal(asc[end-1].created) {
			start--
		}
		out = append(out, asc[start:end]...)
		end = start
	}
	if found {
		s.spineStore(agentID, st, out)
	}
	return out, true
}

// idxPartition returns the spine split into the three populations the ranking
// treats differently, plus the id -> position map, all read-only.
//
// Live content is the ranking corpus and its order IS the recency ranking;
// aliases are vocabulary; tombstones are neither, and are read only for the
// lineage a receipt may ask about. None of that depends on the query, so none
// of it belongs in the per-query path.
func (s *Store) idxPartition(agentID string) (spinePartition, bool) {
	spine, ok := s.idxSpineOrdered(agentID)
	if !ok {
		return spinePartition{}, false
	}
	if p, hit := s.partitionCached(agentID, spine); hit {
		return p, true
	}
	p := spinePartition{live: make([]idxSpineEntry, 0, len(spine))}
	for _, e := range spine {
		switch {
		case e.flags&idxFlagAlias != 0:
			if e.flags&idxFlagSuperseded == 0 {
				p.aliasIDs = append(p.aliasIDs, e.id)
			}
		case e.flags&idxFlagSuperseded != 0:
			p.tombIDs = append(p.tombIDs, e.id)
		default:
			p.live = append(p.live, e)
		}
	}
	p.index = make(map[string]int, len(p.live))
	for i := range p.live {
		p.index[p.live[i].id] = i
	}
	s.partitionStore(agentID, spine, p)
	return p, true
}

// spinePartition is one agent's spine, already split and indexed. Everything in
// it is read-only for callers.
type spinePartition struct {
	live     []idxSpineEntry
	index    map[string]int
	aliasIDs []string
	tombIDs  []string
}

// partitionCached keys on the identity of the spine slice it was derived from.
// The spine is itself cached against the index write counter, so a spine that
// has not been rebuilt is the same backing array — which makes this check a
// pointer comparison rather than a second stamp to keep in sync.
func (s *Store) partitionCached(agentID string, spine []idxSpineEntry) (spinePartition, bool) {
	s.spineMu.RLock()
	defer s.spineMu.RUnlock()
	c, ok := s.partitions[agentID]
	if !ok || len(c.spine) != len(spine) {
		return spinePartition{}, false
	}
	if len(spine) > 0 && &c.spine[0] != &spine[0] {
		return spinePartition{}, false
	}
	return c.part, true
}

func (s *Store) partitionStore(agentID string, spine []idxSpineEntry, p spinePartition) {
	s.spineMu.Lock()
	defer s.spineMu.Unlock()
	if s.partitions == nil {
		s.partitions = make(map[string]partitionSnapshot, 1)
	}
	s.partitions[agentID] = partitionSnapshot{spine: spine, part: p}
}

type partitionSnapshot struct {
	spine []idxSpineEntry
	part  spinePartition
}

// spineCached returns the cached spine when it was built from exactly this
// index state. Callers must treat the slice as read-only — the recall pipeline
// copies what it needs into its own slices, and a caller that mutated this one
// would corrupt every later query in the process.
func (s *Store) spineCached(agentID string, st indexState) ([]idxSpineEntry, bool) {
	s.spineMu.RLock()
	defer s.spineMu.RUnlock()
	c, ok := s.spine[agentID]
	if !ok || c.writes != st.Writes || c.facts != st.Facts || c.stemmed != st.Stemmed {
		return nil, false
	}
	return c.entries, true
}

func (s *Store) spineStore(agentID string, st indexState, entries []idxSpineEntry) {
	s.spineMu.Lock()
	defer s.spineMu.Unlock()
	if s.spine == nil {
		s.spine = make(map[string]spineSnapshot, 1)
	}
	s.spine[agentID] = spineSnapshot{
		writes:  st.Writes,
		facts:   st.Facts,
		stemmed: st.Stemmed,
		entries: entries,
	}
}

// spineSnapshot is one agent's ordered spine plus the index state it was built
// from. All three stamp fields are compared, not just the counter: a store
// reopened under a different tokenisation fold rebuilds the index, and a cache
// that only watched the counter would answer from postings that mean something
// else.
type spineSnapshot struct {
	writes  uint64
	facts   int
	stemmed bool
	entries []idxSpineEntry
}

// idxFetch loads the lite form of the given IDs.
func (s *Store) idxFetch(agentID string, ids []string) ([]Fact, error) {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	m := make(map[string]Fact, len(ids))
	if err := s.idxFetchInto(agentID, set, m); err != nil {
		return nil, err
	}
	out := make([]Fact, 0, len(m))
	for _, id := range ids {
		if f, hit := m[id]; hit {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *Store) idxFetchInto(agentID string, ids map[string]bool, dst map[string]Fact) error {
	if len(ids) == 0 {
		return nil
	}
	return s.db.View(func(tx *bolt.Tx) error {
		got, err := s.idxLoadFacts(tx, agentID, ids)
		if err != nil {
			return err
		}
		for id, f := range got {
			dst[id] = f
		}
		return nil
	})
}

// idxLoadHead reads the texts the caller is about to walk. RecallDetailed
// dedups by text, so a window of exactly topK entries can come up short; this
// widens the window until topK distinct texts are in hand.
func (s *Store) idxLoadHead(agentID string, ranked []scored, topK int, dst map[string]Fact) error {
	if topK <= 0 || len(ranked) == 0 {
		return nil
	}
	window := topK
	for {
		if window > len(ranked) {
			window = len(ranked)
		}
		missing := make(map[string]bool)
		for _, sc := range ranked[:window] {
			if _, have := dst[sc.id]; !have {
				missing[sc.id] = true
			}
		}
		if err := s.idxFetchInto(agentID, missing, dst); err != nil {
			return err
		}
		distinct := make(map[string]bool, window)
		for _, sc := range ranked[:window] {
			if f, have := dst[sc.id]; have {
				distinct[f.Text] = true
			}
		}
		if len(distinct) >= topK || window >= len(ranked) {
			return nil
		}
		window *= 2
	}
}

// idxDFCached answers a document-frequency question, remembering the answer:
// the neighbourhood asks about every co-occurring term it considers, and each
// miss would otherwise be a fresh transaction.
func (s *Store) idxDFCached(agentID, term string, cache map[string]int) int {
	if v, hit := cache[term]; hit {
		return v
	}
	v := 0
	_ = s.db.View(func(tx *bolt.Tx) error {
		v = idxDF(idxBucketRO(tx, bucketIdxTerms, agentID), term)
		return nil
	})
	cache[term] = v
	return v
}

// idxSeedDocs is the one-hop neighbourhood over posting lists: the facts
// containing at least one seed term, which is exactly what the scan path
// selects by walking the whole corpus and testing each fact.
func (s *Store) idxSeedDocs(agentID string, seeds map[string]bool, liveIdx map[string]int, doStem bool) []map[string]int {
	ids := make(map[string]bool)
	_ = s.db.View(func(tx *bolt.Tx) error {
		tb := idxBucketRO(tx, bucketIdxTerms, agentID)
		for t := range seeds {
			idxPostings(tb, t, ids)
		}
		return nil
	})
	for id := range ids {
		if _, isLive := liveIdx[id]; !isLive {
			delete(ids, id)
		}
	}
	facts := make(map[string]Fact, len(ids))
	if err := s.idxFetchInto(agentID, ids, facts); err != nil {
		return nil
	}
	out := make([]map[string]int, 0, len(facts))
	for _, f := range facts {
		toks := tokenizeStem(f.Text, doStem)
		tf := make(map[string]int, len(toks))
		for _, t := range toks {
			tf[t]++
		}
		out = append(out, tf)
	}
	return out
}
