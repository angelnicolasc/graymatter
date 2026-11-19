package memory

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
)

const (
	maxRecallReviewNotices = 4
	maxRecallReviewSources = 12
)

// MergeReviewNotices combines notices in first-seen order. It bounds the
// whole result, including multi-query and multi-namespace recall.
func MergeReviewNotices(groups ...[]ReviewNotice) []ReviewNotice {
	var result []ReviewNotice
	index := make(map[string]int)
	for _, notices := range groups {
		for _, notice := range notices {
			if !reviewNoticeID(notice.ID) || (notice.SourceState != "current" && reviewStateRank(notice.SourceState) == 0) {
				continue
			}
			pos, found := index[notice.ID]
			if !found && len(result) >= maxRecallReviewNotices {
				continue
			}
			var ids []string
			if found {
				ids = result[pos].SourceIDs
			}
			for _, id := range notice.SourceIDs {
				if len(ids) == maxRecallReviewSources {
					break
				}
				if reviewNoticeID(id) && !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
			if len(ids) == 0 {
				continue
			}
			if !found {
				index[notice.ID] = len(result)
				result = append(result, ReviewNotice{ID: notice.ID, SourceIDs: ids, SourceState: notice.SourceState})
				continue
			}
			result[pos].SourceIDs = ids
			if reviewStateRank(notice.SourceState) > reviewStateRank(result[pos].SourceState) {
				result[pos].SourceState = notice.SourceState
			}
		}
	}
	return result
}

func reviewNoticeID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// ReviewNoticesText renders the same bounded metadata as the structured
// result. Provider notes and candidate text stay inside the review journal.
func ReviewNoticesText(notices []ReviewNotice) string {
	notices = MergeReviewNotices(notices)
	if len(notices) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("Open memory conflicts")
	for _, notice := range notices {
		fmt.Fprintf(&out, "\n- Conflict %q has %s source state and affects ", notice.ID, notice.SourceState)
		for i, id := range notice.SourceIDs {
			if i > 0 {
				out.WriteString(", ")
			}
			fmt.Fprintf(&out, "%q", id)
		}
		out.WriteByte('.')
	}
	return out.String()
}

func (s *Store) recalledReviewNotices(ctx context.Context, facts []Fact) []ReviewNotice {
	if len(facts) == 0 {
		return nil
	}
	var namespaces []string
	ids := make(map[string][]string)
	for _, fact := range facts {
		if _, exists := ids[fact.AgentID]; !exists {
			namespaces = append(namespaces, fact.AgentID)
		}
		ids[fact.AgentID] = append(ids[fact.AgentID], fact.ID)
	}
	var result []ReviewNotice
	for _, namespace := range namespaces {
		notices, err := s.ReviewNotices(ctx, namespace, ids[namespace])
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("graymatter: read conflict notices: %v", err)
			}
			continue
		}
		result = MergeReviewNotices(result, notices)
	}
	return result
}

func (s *Store) annotateReviewReceipts(ctx context.Context, receipts []RecallReceipt, selected []Fact) {
	for _, notice := range s.recalledReviewNotices(ctx, selected) {
		for i := range receipts {
			if slices.Contains(notice.SourceIDs, receipts[i].Provenance.FactID) {
				receipts[i].ReviewNotices = append(receipts[i].ReviewNotices, notice)
				break
			}
		}
	}
}
