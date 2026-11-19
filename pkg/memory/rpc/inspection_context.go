package rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	netrpc "net/rpc"
	"sync/atomic"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// InspectionContextV1 is negotiated separately: older inspection endpoints
// silently ignore additional request fields and cannot safely honor cancellation.
const InspectionContextV1 = "inspection-context-v1"
const maxInspectionDuration = 30 * time.Second
const maxReviewAnalyzeDuration = 90 * time.Second
const maxInspectionCalls = 1024
const maxClientInspectionCalls = 64
const maxClientInspectionCancels = 16

// ErrInspectionBusy means no request was dispatched because the client's
// bounded inspection transport capacity is occupied by unfinished calls.
var ErrInspectionBusy = errors.New("rpc: inspection transport busy; retry after pending requests finish")

// InspectionContext carries no credentials. Request IDs are random, single-use
// cancellation capabilities; deadlines use UTC Unix nanoseconds across processes.
type InspectionContext struct {
	RequestID        string
	DeadlineUnixNano int64
	ReviewAnalyze    bool `json:",omitempty"`
}
type CancelInspectionRequest struct{ Context InspectionContext }
type CancelInspectionResponse struct{}
type inspectionCallState struct {
	cancel   context.CancelFunc
	deadline time.Time
	canceled bool
	token    *byte
	timer    *time.Timer
}

func inspectionDeadline(meta InspectionContext, now time.Time) (time.Time, error) {
	if len(meta.RequestID) != 32 {
		return time.Time{}, errors.New("rpc: invalid inspection request ID")
	}
	if _, err := hex.DecodeString(meta.RequestID); err != nil {
		return time.Time{}, errors.New("rpc: invalid inspection request ID")
	}
	if meta.DeadlineUnixNano <= 0 {
		return time.Time{}, errors.New("rpc: inspection deadline required")
	}
	deadline := time.Unix(0, meta.DeadlineUnixNano)
	if !deadline.After(now) {
		return time.Time{}, context.DeadlineExceeded
	}
	duration := maxInspectionDuration
	if meta.ReviewAnalyze {
		duration = maxReviewAnalyzeDuration
	}
	if bound := now.Add(duration); deadline.After(bound) {
		deadline = bound
	}
	return deadline, nil
}

// pruneInspectionLocked bounds both active calls and cancel-before-start
// tombstones. The context's own deadline timer cancels expired active calls.
func (s *Server) pruneInspectionLocked(now time.Time) {
	for id, call := range s.inspectionCalls {
		if !call.deadline.After(now) {
			if call.timer != nil {
				call.timer.Stop()
			}
			delete(s.inspectionCalls, id)
		}
	}
}

// setInspectionLocked arranges cleanup even when this is the last request the
// server receives. Tokens prevent a late callback deleting a newer ID entry.
func (s *Server) setInspectionLocked(id string, call inspectionCallState) {
	if old := s.inspectionCalls[id]; old.timer != nil {
		old.timer.Stop()
	}
	call.token = new(byte)
	token := call.token
	call.timer = time.AfterFunc(time.Until(call.deadline), func() {
		s.inspectionMu.Lock()
		defer s.inspectionMu.Unlock()
		if current, ok := s.inspectionCalls[id]; ok && current.token == token {
			delete(s.inspectionCalls, id)
		}
	})
	s.inspectionCalls[id] = call
}

func (s *Server) beginInspection(meta InspectionContext) (context.Context, context.CancelFunc, error) {
	return s.beginInspectionOperation(meta, false)
}

func (s *Server) beginInspectionOperation(meta InspectionContext, analyze bool) (context.Context, context.CancelFunc, error) {
	if meta.ReviewAnalyze != analyze {
		return nil, nil, errors.New("rpc: unexpected review analysis context")
	}
	now := time.Now()
	deadline, err := inspectionDeadline(meta, now)
	if err != nil {
		return nil, nil, err
	}
	s.inspectionMu.Lock()
	defer s.inspectionMu.Unlock()
	select {
	case <-s.stop:
		return nil, nil, context.Canceled
	default:
	}
	s.pruneInspectionLocked(now)
	if s.inspectionCalls == nil {
		s.inspectionCalls = make(map[string]inspectionCallState)
	}
	old, exists := s.inspectionCalls[meta.RequestID]
	if exists && old.cancel != nil {
		return nil, nil, errors.New("rpc: duplicate inspection request ID")
	}
	if !exists && len(s.inspectionCalls) >= maxInspectionCalls {
		return nil, nil, errors.New("rpc: too many inspection requests")
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	if old.canceled {
		cancel()
	}
	s.setInspectionLocked(meta.RequestID, inspectionCallState{cancel: cancel, deadline: deadline, canceled: old.canceled})
	token := s.inspectionCalls[meta.RequestID].token
	finish := func() {
		cancel()
		s.inspectionMu.Lock()
		if current, ok := s.inspectionCalls[meta.RequestID]; ok && current.token == token {
			current.timer.Stop()
			delete(s.inspectionCalls, meta.RequestID)
		}
		s.inspectionMu.Unlock()
	}
	return ctx, finish, nil
}

// CancelInspection can race method dispatch because net/rpc handlers run in
// separate goroutines. A bounded, expiring tombstone also cancels a later start.
func (s *Server) CancelInspection(req *CancelInspectionRequest, _ *CancelInspectionResponse) error {
	deadline, err := inspectionDeadline(req.Context, time.Now())
	if err != nil {
		return err
	}
	s.inspectionMu.Lock()
	defer s.inspectionMu.Unlock()
	select {
	case <-s.stop:
		return context.Canceled
	default:
	}
	s.pruneInspectionLocked(time.Now())
	if s.inspectionCalls == nil {
		s.inspectionCalls = make(map[string]inspectionCallState)
	}
	call, exists := s.inspectionCalls[req.Context.RequestID]
	if !exists && len(s.inspectionCalls) >= maxInspectionCalls {
		return errors.New("rpc: too many inspection requests")
	}
	if call.cancel != nil {
		call.cancel()
		return nil
	}
	// Repeated cancellation cannot extend a tombstone's lifetime.
	if !exists || deadline.Before(call.deadline) {
		call.deadline = deadline
	}
	call.canceled = true
	s.setInspectionLocked(req.Context.RequestID, call)
	return nil
}

func (s *Server) cancelInspections() {
	s.inspectionMu.Lock()
	defer s.inspectionMu.Unlock()
	for id, call := range s.inspectionCalls {
		if call.cancel != nil {
			call.cancel()
		}
		if call.timer != nil {
			call.timer.Stop()
		}
		delete(s.inspectionCalls, id)
	}
}

func (c *Client) inspectionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	bound := c.callTimeout
	if bound <= 0 || bound > maxInspectionDuration {
		bound = maxInspectionDuration
	}
	return context.WithTimeout(ctx, bound)
}

func newInspectionContext(ctx context.Context) (InspectionContext, error) {
	if err := ctx.Err(); err != nil {
		return InspectionContext{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return InspectionContext{}, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return InspectionContext{}, errors.New("rpc: inspection context has no deadline")
	}
	return InspectionContext{RequestID: hex.EncodeToString(id[:]), DeadlineUnixNano: deadline.UnixNano()}, nil
}

func (c *Client) initInspectionSlots() {
	c.inspectionOnce.Do(func() {
		c.inspectionSendSlots = make(chan struct{}, maxClientInspectionCalls)
		c.inspectionCancelSlots = make(chan struct{}, maxClientInspectionCancels)
	})
}

// Cancellation is best effort; the mandatory server deadline remains the bound
// when the transport is broken. A broken client cannot consume another client's
// cancellation capacity. Reservations live until the underlying Call completes.
func (c *Client) cancelInspection(meta *InspectionContext) {
	if meta == nil {
		return
	}
	c.initInspectionSlots()
	select {
	case c.inspectionCancelSlots <- struct{}{}:
		go func() {
			defer func() { <-c.inspectionCancelSlots }()
			_ = c.rpc.Call(ServiceName+".CancelInspection", &CancelInspectionRequest{Context: *meta}, &CancelInspectionResponse{})
		}()
	default:
	}
}

// rpcInspectionCall owns its response until completion. Returning on cancellation
// must never read a response that the RPC codec may still be writing. Dispatch
// runs in a goroutine because net/rpc.Go itself may block on a shared writer.
func rpcInspectionCall[T any](c *Client, ctx context.Context, method string, req any, meta *InspectionContext, mutation bool) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	c.initInspectionSlots()
	select {
	case c.inspectionSendSlots <- struct{}{}:
	default:
		return zero, ErrInspectionBusy
	}
	response := new(T)
	done := make(chan error, 1)
	var state atomic.Int32 // 0 pending, 1 potentially dispatched, 2 canceled before dispatch
	go func() {
		defer func() { <-c.inspectionSendSlots }()
		if err := ctx.Err(); err != nil {
			done <- err
			return
		}
		if !state.CompareAndSwap(0, 1) {
			done <- ctx.Err()
			return
		}
		done <- c.rpc.Call(ServiceName+"."+method, req, response)
	}()
	finished := func(err error) (T, error) {
		if err != nil && mutation && state.Load() == 1 {
			var serverError netrpc.ServerError
			if !errors.As(err, &serverError) {
				return zero, fmt.Errorf("%w: %v", memory.ErrMutationOutcomeUnknown, err)
			}
		}
		return *response, err
	}
	select {
	case err := <-done:
		return finished(err)
	case <-ctx.Done():
		// A complete response is authoritative even when the caller's
		// cancellation became ready at the same time.
		select {
		case err := <-done:
			return finished(err)
		default:
		}
		if !state.CompareAndSwap(0, 2) && state.Load() == 1 {
			c.cancelInspection(meta)
			if mutation {
				return zero, fmt.Errorf("%w: %v", memory.ErrMutationOutcomeUnknown, ctx.Err())
			}
		}
		return zero, ctx.Err()
	}
}
