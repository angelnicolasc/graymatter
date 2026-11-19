package rpc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// ReviewJournalV1 advertises explicit, versioned proposal journal operations.
const ReviewJournalV1 = "review-journal-v1"

var ErrReviewUnsupported = errors.New("review journal unsupported")

// ReviewBackend is optional. Implementing it does not affect maintenance or
// turn on an automatic analysis path.
type ReviewBackend interface {
	ReviewAnalyze(context.Context, string, memory.ReviewAnalyzeOptions) ([]memory.ReviewRecord, error)
	ReviewList(context.Context, string) ([]memory.ReviewRecord, error)
	ReviewInspect(context.Context, string, string) (memory.ReviewRecord, error)
	ReviewApply(context.Context, string, string, memory.ReviewAction) error
}

var _ ReviewBackend = (*memory.Store)(nil)

func (s *Server) reviewBackend() ReviewBackend {
	if owner, ok := s.backend.(interface{ ReviewBackend() ReviewBackend }); ok {
		return owner.ReviewBackend()
	}
	backend, _ := s.backend.(ReviewBackend)
	return backend
}

type ReviewAnalyzeRequest struct {
	Context                    InspectionContext
	Namespace, Provider, Model string
}
type ReviewListRequest struct {
	Context   InspectionContext
	Namespace string
}
type ReviewInspectRequest struct {
	Context       InspectionContext
	Namespace, ID string
}
type ReviewApplyRequest struct {
	Context       InspectionContext
	Namespace, ID string
	Action        memory.ReviewAction
}
type ReviewRecordsResponse struct{ Records []memory.ReviewRecord }
type ReviewInspectResponse struct{ Record memory.ReviewRecord }
type ReviewApplyResponse struct{}

func (s *Server) beginReview(meta InspectionContext, analyze bool) (ReviewBackend, context.Context, context.CancelFunc, error) {
	backend := s.reviewBackend()
	if backend == nil {
		return nil, nil, nil, ErrReviewUnsupported
	}
	ctx, finish, err := s.beginInspectionOperation(meta, analyze)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		finish()
		return nil, nil, nil, err
	}
	return backend, ctx, finish, nil
}

func (s *Server) ReviewAnalyze(req *ReviewAnalyzeRequest, resp *ReviewRecordsResponse) error {
	defer s.touch()
	backend, ctx, finish, err := s.beginReview(req.Context, true)
	if err != nil {
		return err
	}
	defer finish()
	options, err := memory.ReviewOptionsFromConfig(s.consolidateCfg, req.Provider, req.Model)
	if err != nil {
		return err
	}
	resp.Records, err = backend.ReviewAnalyze(ctx, req.Namespace, options)
	return err
}

func (s *Server) ReviewList(req *ReviewListRequest, resp *ReviewRecordsResponse) error {
	defer s.touch()
	backend, ctx, finish, err := s.beginReview(req.Context, false)
	if err != nil {
		return err
	}
	defer finish()
	resp.Records, err = backend.ReviewList(ctx, req.Namespace)
	return err
}

func (s *Server) ReviewInspect(req *ReviewInspectRequest, resp *ReviewInspectResponse) error {
	defer s.touch()
	backend, ctx, finish, err := s.beginReview(req.Context, false)
	if err != nil {
		return err
	}
	defer finish()
	resp.Record, err = backend.ReviewInspect(ctx, req.Namespace, req.ID)
	return err
}

func (s *Server) ReviewApply(req *ReviewApplyRequest, _ *ReviewApplyResponse) error {
	defer s.touch()
	backend, ctx, finish, err := s.beginReview(req.Context, false)
	if err != nil {
		return err
	}
	defer finish()
	return backend.ReviewApply(ctx, req.Namespace, req.ID, req.Action)
}

// ReviewJournalSupported reports whether the current peer negotiated the
// journal and its request-context contract.
func (c *Client) ReviewJournalSupported() bool {
	journal, _ := c.Capability(ReviewJournalV1)
	cancellation, _ := c.Capability(InspectionContextV1)
	return journal && cancellation
}

func reviewRPCError(err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{
		ErrReviewUnsupported, memory.ErrReviewNotFound, memory.ErrReviewQuota,
		memory.ErrReviewInvalid, memory.ErrReviewTransition, memory.ErrStoreReadOnly,
		memory.ErrReviewNoSources, memory.ErrInvalidReviewResponse, memory.ErrFactChanged,
		context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, sentinel) || err.Error() == sentinel.Error() {
			return sentinel
		}
		if detail, ok := strings.CutPrefix(err.Error(), sentinel.Error()+": "); ok {
			return fmt.Errorf("%w: %s", sentinel, detail)
		}
	}
	return err
}

func callReview[T any](c *Client, ctx context.Context, method string, request func(InspectionContext) any, mutation bool) (T, error) {
	var zero T
	preflight, stopPreflight := c.inspectionContext(ctx)
	err := c.requireInspection(preflight, ReviewJournalV1)
	stopPreflight()
	if err != nil {
		if errors.Is(err, memory.ErrInspectionUnsupported) {
			return zero, fmt.Errorf("%w: update and restart the daemon", ErrReviewUnsupported)
		}
		return zero, err
	}
	analyze := method == "ReviewAnalyze"
	var cancel context.CancelFunc
	if analyze {
		ctx, cancel = context.WithTimeout(ctx, maxReviewAnalyzeDuration)
	} else {
		ctx, cancel = c.inspectionContext(ctx)
	}
	defer cancel()
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return zero, err
	}
	meta.ReviewAnalyze = analyze
	resp, err := rpcInspectionCall[T](c, ctx, method, request(meta), &meta, mutation)
	return resp, reviewRPCError(err)
}

func (c *Client) ReviewAnalyze(ctx context.Context, namespace, provider, model string) ([]memory.ReviewRecord, error) {
	resp, err := callReview[ReviewRecordsResponse](c, ctx, "ReviewAnalyze", func(meta InspectionContext) any {
		return &ReviewAnalyzeRequest{Context: meta, Namespace: namespace, Provider: provider, Model: model}
	}, true)
	return resp.Records, err
}

func (c *Client) ReviewList(ctx context.Context, namespace string) ([]memory.ReviewRecord, error) {
	resp, err := callReview[ReviewRecordsResponse](c, ctx, "ReviewList", func(meta InspectionContext) any {
		return &ReviewListRequest{Context: meta, Namespace: namespace}
	}, false)
	return resp.Records, err
}

func (c *Client) ReviewInspect(ctx context.Context, namespace, id string) (memory.ReviewRecord, error) {
	resp, err := callReview[ReviewInspectResponse](c, ctx, "ReviewInspect", func(meta InspectionContext) any {
		return &ReviewInspectRequest{Context: meta, Namespace: namespace, ID: id}
	}, false)
	return resp.Record, err
}

func (c *Client) ReviewApply(ctx context.Context, namespace, id string, action memory.ReviewAction) error {
	_, err := callReview[ReviewApplyResponse](c, ctx, "ReviewApply", func(meta InspectionContext) any {
		return &ReviewApplyRequest{Context: meta, Namespace: namespace, ID: id, Action: action}
	}, true)
	return err
}
