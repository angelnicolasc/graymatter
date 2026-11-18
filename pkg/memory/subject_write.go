package memory

import (
	"encoding/json"
	"fmt"
	"sort"

	bolt "go.etcd.io/bbolt"
)

type subjectAssignment struct {
	Key, Name string
	Prior     *resolvedSubject
}
type subjectWritePlan struct {
	Sources     []Fact
	Assignments []subjectAssignment
	Suggestions SuggestionResult
}

func subjectWritePlansTx(tx *bolt.Tx, namespace string, inputs []WriteFact, suggest bool) ([]subjectWritePlan, error) {
	plans := make([]subjectWritePlan, len(inputs))
	needIndex := false
	for _, in := range inputs {
		needIndex = needIndex || in.Options.HasSubjectIntent()
	}
	idx := subjectIndex{Heads: map[string]resolvedSubject{}, ByID: map[string][]string{}}
	var err error
	if needIndex {
		idx, err = resolveSubjectIndexTx(tx, namespace)
		if err != nil {
			return nil, err
		}
	}
	var initial []Fact
	if suggest {
		initial, err = canonicalFactsTx(tx, namespace)
		if err != nil {
			return nil, err
		}
	}
	usedIDs := map[string]bool{}
	usedKeys := map[string]bool{}
	for i, in := range inputs {
		plan := &plans[i]
		if in.Options.Subject != nil {
			key, err := NormalizeSubject(*in.Options.Subject)
			if err != nil {
				return nil, err
			}
			assignment := subjectAssignment{Key: key, Name: *in.Options.Subject}
			prior, exists := idx.Heads[key]
			if in.Options.Replaces != nil && (!exists || prior.Unresolved == nil) {
				return nil, fmt.Errorf("subject and replaces are alternatives")
			}
			if exists {
				assignment.Prior = &prior
				if prior.Unresolved != nil {
					if in.Options.Replaces == nil {
						return nil, fmt.Errorf("%w; recover with memory_add using subject and replaces: [live fact ID, or recorded head ID %q if no live fact remains]", prior.Unresolved, prior.Head.Reference.FactID)
					}
					plan.Sources, err = subjectRecoverySourcesTx(tx, namespace, idx, prior, in.Options.Replaces[0])
					if err != nil {
						return nil, err
					}
				} else if !prior.Closed {
					if len(idx.ByID[prior.Fact.ID]) != 1 {
						return nil, fmt.Errorf("%w: subject %q shares fact %s with %v", ErrSubjectAmbiguous, *in.Options.Subject, prior.Fact.ID, idx.ByID[prior.Fact.ID])
					}
					// The head resolver needs only ranking/version fields, but
					// PR-R validates the complete mutation snapshot (including
					// PinnedAt). Read that snapshot inside this same write tx.
					current, err := factInTx(tx, namespace, prior.Fact.ID)
					if err != nil {
						return nil, err
					}
					plan.Sources = []Fact{current}
				}
			}
			plan.Assignments = []subjectAssignment{assignment}
		} else if in.Options.Replaces != nil {
			keys := map[string]bool{}
			for _, id := range in.Options.Replaces {
				fact, err := resolveFactIDTx(tx, namespace, id, false)
				if err != nil {
					return nil, err
				}
				if fact.IsAlias() {
					return nil, fmt.Errorf("replaces cannot turn an alias into an ordinary fact")
				}
				plan.Sources = append(plan.Sources, fact)
				for _, key := range idx.ByID[fact.ID] {
					keys[key] = true
				}
			}
			if len(keys) > 1 && len(plan.Sources) > 1 {
				return nil, fmt.Errorf("%w: replaces would merge distinct subject heads", ErrSubjectAmbiguous)
			}
			var ordered []string
			for key := range keys {
				ordered = append(ordered, key)
			}
			sort.Strings(ordered)
			for _, key := range ordered {
				prior := idx.Heads[key]
				plan.Assignments = append(plan.Assignments, subjectAssignment{Key: key, Name: prior.Head.Name, Prior: &prior})
			}
		} else if suggest {
			plan.Suggestions = similarFactSuggestions(namespace, in.Text, initial)
		}
		for _, source := range plan.Sources {
			if usedIDs[source.ID] {
				return nil, fmt.Errorf("batch replacement targets overlap at fact %s", source.ID)
			}
			usedIDs[source.ID] = true
		}
		for _, assignment := range plan.Assignments {
			if usedKeys[assignment.Key] {
				return nil, fmt.Errorf("batch writes the same normalized subject more than once")
			}
			usedKeys[assignment.Key] = true
		}
	}
	return plans, nil
}

func subjectRecoverySourcesTx(tx *bolt.Tx, namespace string, idx subjectIndex, prior resolvedSubject, id string) ([]Fact, error) {
	rows := lineageRowBucket(tx, namespace)
	anchorSelected := id == prior.Head.Reference.FactID
	if anchorSelected && (rows == nil || rows.Get([]byte(id)) == nil) {
		return nil, nil
	}
	fact, err := factInTx(tx, namespace, id)
	if anchorSelected && err == nil && fact.IsSuperseded() {
		live, err := observedLiveRetirementTarget(rows, fact)
		if err != nil {
			return nil, err
		}
		if live {
			return nil, fmt.Errorf("%w: a live row still follows the recorded head; name the exact live fact ID in replaces", prior.Unresolved)
		}
		// A reset names the stored anchor exactly. Its retired or missing
		// canonical row is never a revision source and never made live.
		return nil, nil
	}
	if err != nil || fact.IsSuperseded() || fact.IsAlias() {
		return nil, fmt.Errorf("%w: recovery requires an exact live fact ID, or the recorded head ID %q if no live fact remains", prior.Unresolved, prior.Head.Reference.FactID)
	}
	if len(idx.ByID[fact.ID]) != 0 {
		return nil, fmt.Errorf("%w: recovery fact %q belongs to another subject", ErrSubjectAmbiguous, id)
	}
	return []Fact{fact}, nil
}

// Check observable pointers only to reject an anchor reset while a live
// replacement remains. They are never used to select a revision victim.
func observedLiveRetirementTarget(rows *bolt.Bucket, anchor Fact) (bool, error) {
	seen := map[string]bool{anchor.ID: true}
	for id := anchor.SupersededBy; id != "" && id != SupersededByAgent && !seen[id]; {
		seen[id] = true
		raw := rows.Get([]byte(id))
		if raw == nil {
			return false, nil
		}
		fact, err := unmarshalFactLite(raw)
		if err != nil {
			return false, err
		}
		if fact.ID != id || fact.AgentID != anchor.AgentID {
			return false, fmt.Errorf("retirement target identity mismatch")
		}
		if !fact.IsSuperseded() {
			return true, nil
		}
		id = fact.SupersededBy
	}
	return false, nil
}

func (s *Store) applySubjectWriteTx(tx *bolt.Tx, fact *Fact, options WriteOptions, plan subjectWritePlan, added *int64) (WriteReceipt, bool, error) {
	receipt := WriteReceipt{Suggestions: plan.Suggestions.Suggestions, SuggestionsTotal: plan.Suggestions.Total}
	collect := false
	if len(plan.Sources) > 0 {
		if len(plan.Sources) == 1 && len(plan.Assignments) == 1 {
			prior := plan.Assignments[0].Prior
			if prior != nil && prior.DamagedSnapshot != nil {
				if err := repairSubjectSourceSnapshotTx(tx, *prior.DamagedSnapshot, plan.Sources[0], added); err != nil {
					return receipt, false, err
				}
			}
		}
		validated, dropped, err := s.prepareRevisionTx(tx, fact, options, plan.Sources, added)
		if err != nil {
			return receipt, false, err
		}
		collect = dropped
		for _, old := range validated {
			receipt.Replaced = append(receipt.Replaced, ReplacedFact{ID: old.ID, Text: old.Text})
		}
	} else if err := s.recordCitationTx(tx, fact, options, added); err != nil {
		return receipt, false, err
	}
	for _, assignment := range plan.Assignments {
		if err := s.advanceSubjectTx(tx, fact.AgentID, assignment.Key, assignment.Name, assignment.Prior, fact, added); err != nil {
			return receipt, false, err
		}
		revisions := uint64(0)
		if assignment.Prior != nil {
			revisions = assignment.Prior.Revisions + 1
		}
		fact.Subjects = append(fact.Subjects, SubjectProjection{Name: assignment.Name, Revisions: revisions})
	}
	receipt.Fact = *fact
	receipt.Subjects = append([]SubjectProjection(nil), fact.Subjects...)
	return receipt, collect, nil
}

// Explicit recovery has already selected the canonical row in this write
// transaction. Repair only a damaged snapshot whose exact identity and hash
// that row proves. All ordinary revision checks and quotas still apply.
func repairSubjectSourceSnapshotTx(tx *bolt.Tx, damaged LineageReference, source Fact, added *int64) error {
	snapshot := NewEvidenceSnapshot(source)
	if lineageRef(snapshot) != damaged {
		return nil
	}
	snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, source.AgentID, true)
	if err != nil {
		return err
	}
	key := []byte(snapshot.key())
	previous := snapshots.Get(key)
	repaired, err := json.Marshal(storedEvidence{Snapshot: snapshot})
	if err != nil {
		return err
	}
	delta := len(repaired) - len(previous)
	if previous == nil {
		delta += len(key)
	}
	if delta > 0 {
		*added += int64(delta)
	}
	return snapshots.Put(key, repaired)
}
