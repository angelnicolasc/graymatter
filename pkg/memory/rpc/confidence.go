package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Capability versions cover option validation, the ranking formula and receipts.
// Additive wire contracts leave Protocol and the legacy endpoints compatible.
const (
	ConfidenceWriteV1       = "confidence-write-v1"
	ConfidenceRecallV1      = "confidence-recall-v1"
	ConfidenceLifecycleV1   = "confidence-lifecycle-v1"
	ConfidenceSharedWriteV1 = "confidence-shared-write-v1"
	FactIdentityV1          = "fact-identity-v1"
)

// ErrUnsupportedCapability is semantic: reconnecting cannot make the request
// safe, and callers must never replay a mutation after this rejection.
var ErrUnsupportedCapability = memory.ErrConfidenceUnsupported

// ConfidenceWriter and ConfidenceRecaller are additive store capabilities.
// Keeping them separate from Backend preserves older Go implementations.
type ConfidenceWriter interface {
	PutWithOptionsReturningFact(context.Context, string, string, memory.WriteOptions) (memory.Fact, error)
	ReviseWithOptions(context.Context, string, string, string, memory.WriteOptions) (memory.Fact, error)
	ReviseFactsWithOptions(context.Context, string, string, memory.WriteOptions, ...memory.Fact) (memory.Fact, error)
}

type ConfidenceLifecycle interface {
	Retire(string, ...memory.Fact) error
	SetPinned(string, bool, ...memory.Fact) error
}

// SharedConfidenceWriter preserves explicit RememberShared semantics: the
// committed fact carries options, while the write schedules no consolidation.
type SharedConfidenceWriter interface {
	PutSharedWithOptionsReturningFact(context.Context, string, memory.WriteOptions) (memory.Fact, error)
}

// DefaultConfidenceProvider reports the effective product default owned by a
// store or negotiated connection, without growing the legacy store contracts.
type DefaultConfidenceProvider interface{ DefaultConfidenceWeight() float64 }

type ConfidenceRecaller interface {
	RecallWithOptions(context.Context, string, string, int, memory.RecallOptions) (memory.RecallResult, error)
	RecallExplainWithOptions(context.Context, string, string, int, memory.RecallOptions) (memory.RecallExplainResult, error)
	RecallAllWithOptions(context.Context, string, string, int, memory.RecallOptions) (memory.RecallResult, error)
}

// confidenceCapabilityProvider lets wrappers report their underlying support
// instead of advertising methods that only return unsupported.
type confidenceCapabilityProvider interface {
	ConfidenceCapabilities() []string
}

type PutWithOptionsRequest struct {
	AgentID string
	Text    string
	Options memory.WriteOptions
}

type PutWithOptionsResponse struct {
	Fact memory.Fact
	// A known committed fact must survive a later-phase backend error.
	// net/rpc otherwise discards the reply when the method returns an error.
	Error     string `json:",omitempty"`
	ErrorCode string `json:",omitempty"`
}

type ReviseWithOptionsRequest struct {
	Context InspectionContext
	AgentID string
	Target  string
	Text    string
	Options memory.WriteOptions
}

type ReviseWithOptionsResponse = PutWithOptionsResponse

type ReviseFactsWithOptionsRequest struct {
	Context InspectionContext
	AgentID string
	Text    string
	Options memory.WriteOptions
	Victims []memory.Fact
}

type RetireRequest struct {
	AgentID string
	Victims []memory.Fact
}
type SetPinnedRequest struct {
	AgentID string
	Pinned  bool
	Victims []memory.Fact
}
type LifecycleResponse struct{}

type RecallWithOptionsRequest struct {
	AgentID string
	Query   string
	TopK    int
	Options memory.RecallOptions
}

type RecallWithOptionsResponse struct{ Result memory.RecallResult }
type RecallExplainWithOptionsResponse struct{ Result memory.RecallExplainResult }

func unsupported(capability string) error {
	return fmt.Errorf("%w: %s; update and restart the daemon", ErrUnsupportedCapability, capability)
}

// mapCapabilityError reconstructs the semantic sentinel lost by JSON-RPC.
func mapCapabilityError(err error) error {
	if err != nil && err.Error() == ErrUnsupportedCapability.Error() {
		return ErrUnsupportedCapability
	}
	if err != nil && strings.HasPrefix(err.Error(), ErrUnsupportedCapability.Error()+":") {
		return fmt.Errorf("%w: %s", ErrUnsupportedCapability, strings.TrimPrefix(err.Error(), ErrUnsupportedCapability.Error()+": "))
	}
	return err
}

// JSON pointers express omission in the Go API; a wire-level null is an
// explicit invalid value and must not silently turn a new request legacy.
func validateWireOptions(data []byte, fields ...string) error {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(data, &request); err != nil {
		return err
	}
	var options json.RawMessage
	for key, value := range request {
		if strings.EqualFold(key, "options") {
			options = value
			break
		}
	}
	if options == nil {
		return nil
	}
	if string(options) == "null" {
		return errors.New("rpc: options must be an object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(options, &object); err != nil {
		return err
	}
	for key, value := range object {
		for _, field := range fields {
			if strings.EqualFold(key, field) && string(value) == "null" {
				return fmt.Errorf("rpc: %s must not be null", field)
			}
		}
	}
	return nil
}

func (r *PutWithOptionsRequest) UnmarshalJSON(data []byte) error {
	if err := validateWireOptions(data, "confidence", "source", "quote", "subject", "replaces"); err != nil {
		return err
	}
	type plain PutWithOptionsRequest
	return json.Unmarshal(data, (*plain)(r))
}

func (r *ReviseWithOptionsRequest) UnmarshalJSON(data []byte) error {
	if err := validateWireOptions(data, "confidence", "source", "quote", "subject", "replaces"); err != nil {
		return err
	}
	type plain ReviseWithOptionsRequest
	return json.Unmarshal(data, (*plain)(r))
}

func (r *ReviseFactsWithOptionsRequest) UnmarshalJSON(data []byte) error {
	if err := validateWireOptions(data, "confidence", "source", "quote", "subject", "replaces"); err != nil {
		return err
	}
	type plain ReviseFactsWithOptionsRequest
	return json.Unmarshal(data, (*plain)(r))
}

func (r *RecallWithOptionsRequest) UnmarshalJSON(data []byte) error {
	if err := validateWireOptions(data, "min_confidence", "confidence_weight"); err != nil {
		return err
	}
	type plain RecallWithOptionsRequest
	return json.Unmarshal(data, (*plain)(r))
}

func (s *Server) confidenceCapabilities() []string {
	if provider, ok := s.backend.(confidenceCapabilityProvider); ok {
		return provider.ConfidenceCapabilities()
	}
	var result []string
	if _, ok := s.backend.(ConfidenceWriter); ok {
		result = append(result, ConfidenceWriteV1)
	}
	if _, ok := s.backend.(ConfidenceRecaller); ok {
		result = append(result, ConfidenceRecallV1)
	}
	if _, ok := s.backend.(ConfidenceLifecycle); ok {
		result = append(result, ConfidenceLifecycleV1)
	}
	if _, ok := s.backend.(SharedConfidenceWriter); ok {
		result = append(result, ConfidenceSharedWriteV1)
	}
	return result
}

func (s *Server) PutSharedWithOptionsReturningFact(req *PutWithOptionsRequest, resp *PutWithOptionsResponse) error {
	defer s.touch()
	if err := s.checkCitationOptions(req.Options); err != nil {
		return err
	}
	if err := req.Options.Validate(); err != nil {
		return err
	}
	writer, ok := s.backend.(SharedConfidenceWriter)
	if !ok || !s.hasConfidenceCapability(ConfidenceSharedWriteV1) {
		return unsupported(ConfidenceSharedWriteV1)
	}
	fact, err := writer.PutSharedWithOptionsReturningFact(context.Background(), req.Text, req.Options)
	resp.Fact = fact
	if err != nil {
		if fact.ID != "" {
			resp.Error = err.Error()
			return nil
		}
		return err
	}
	return nil
}

func (s *Server) hasConfidenceCapability(capability string) bool {
	for _, supported := range s.confidenceCapabilities() {
		if capability == supported {
			return true
		}
	}
	return false
}

// Raw library stores historically treat nonpositive legacy topK as empty.
// Product daemon adapters own a configured default instead. Keep that backend
// distinction at the old endpoints while new methods resolve their defaults.
func (s *Server) rawLegacyEmptyTopK(topK int) bool {
	_, raw := s.backend.(*memory.Store)
	return raw && topK <= 0
}

func (s *Server) PutWithOptionsReturningFact(req *PutWithOptionsRequest, resp *PutWithOptionsResponse) error {
	defer s.touch()
	if err := s.checkCitationOptions(req.Options); err != nil {
		return err
	}
	if err := req.Options.Validate(); err != nil {
		return err
	}
	writer, ok := s.backend.(ConfidenceWriter)
	if !ok || !s.hasConfidenceCapability(ConfidenceWriteV1) {
		return unsupported(ConfidenceWriteV1)
	}
	fact, err := writer.PutWithOptionsReturningFact(context.Background(), req.AgentID, req.Text, req.Options)
	resp.Fact = fact
	if err != nil {
		if fact.ID != "" {
			resp.Error = err.Error()
			return nil
		}
		return err
	}
	resp.Fact = fact
	return nil
}

func (s *Server) ReviseWithOptions(req *ReviseWithOptionsRequest, resp *ReviseWithOptionsResponse) error {
	defer s.touch()
	if subjectRequested(req.Options) {
		return errors.New("subject and replaces are supported only by add; revision already selects its targets")
	}
	if err := s.checkCitationOptions(req.Options); err != nil {
		return err
	}
	if err := req.Options.Validate(); err != nil {
		return err
	}
	writer, ok := s.backend.(ConfidenceWriter)
	if !ok || !s.hasConfidenceCapability(ConfidenceWriteV1) {
		return unsupported(ConfidenceWriteV1)
	}
	if !s.supportsAtomicRevision() {
		return unsupported(AtomicRevisionV1)
	}
	ctx, finish, err := s.beginRevision(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	fact, err := writer.ReviseWithOptions(ctx, req.AgentID, req.Target, req.Text, req.Options)
	return revisionResponse(resp, fact, err, req.Context == (InspectionContext{}))
}

func (s *Server) RecallWithOptions(req *RecallWithOptionsRequest, resp *RecallWithOptionsResponse) error {
	defer s.touch()
	if err := req.Options.Validate(); err != nil {
		return err
	}
	recaller, ok := s.backend.(ConfidenceRecaller)
	if !ok || !s.hasConfidenceCapability(ConfidenceRecallV1) {
		return unsupported(ConfidenceRecallV1)
	}
	result, err := recaller.RecallWithOptions(context.Background(), req.AgentID, req.Query, req.TopK, req.Options)
	if err != nil {
		return err
	}
	resp.Result = result
	return nil
}

func (s *Server) ReviseFactsWithOptions(req *ReviseFactsWithOptionsRequest, resp *ReviseWithOptionsResponse) error {
	defer s.touch()
	if subjectRequested(req.Options) {
		return errors.New("subject and replaces are supported only by add; revision already selects its targets")
	}
	if err := s.checkCitationOptions(req.Options); err != nil {
		return err
	}
	if err := req.Options.Validate(); err != nil {
		return err
	}
	writer, ok := s.backend.(ConfidenceWriter)
	if !ok || !s.hasConfidenceCapability(ConfidenceWriteV1) {
		return unsupported(ConfidenceWriteV1)
	}
	if !s.supportsAtomicRevision() {
		return unsupported(AtomicRevisionV1)
	}
	ctx, finish, err := s.beginRevision(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	fact, err := writer.ReviseFactsWithOptions(ctx, req.AgentID, req.Text, req.Options, req.Victims...)
	return revisionResponse(resp, fact, err, req.Context == (InspectionContext{}))
}

// Older typed clients omitted context metadata. The patched server can honor
// those requests safely because the backend still commits the revision atomically.
func (s *Server) beginRevision(meta InspectionContext) (context.Context, context.CancelFunc, error) {
	if meta == (InspectionContext{}) {
		return context.Background(), func() {}, nil
	}
	return s.beginInspection(meta)
}

func revisionResponse(resp *ReviseWithOptionsResponse, fact memory.Fact, err error, legacy bool) error {
	resp.Fact = fact
	if err != nil {
		if legacy && fact.ID == "" {
			return err
		}
		resp.Error, resp.ErrorCode = err.Error(), revisionErrorCode(err)
	}
	return nil
}

// Preserve typed errors even when the store wraps them with transaction context.
func revisionErrorCode(err error) string {
	switch {
	case errors.Is(err, memory.ErrSubjectUnsupported):
		return "subject_unsupported"
	case errors.Is(err, memory.ErrSubjectAmbiguous):
		return "subject_ambiguous"
	case errors.Is(err, memory.ErrSubjectHeadUnavailable):
		return "subject_head_unavailable"
	case errors.Is(err, memory.ErrFactChanged):
		return "fact_changed"
	case errors.Is(err, memory.ErrRevisionRequired):
		return "revision_required"
	case errors.Is(err, memory.ErrEvidenceQuota):
		return "evidence_quota"
	case errors.Is(err, memory.ErrStoreReadOnly):
		return "read_only"
	case errors.Is(err, memory.ErrInspectionUnsupported):
		return "unsupported"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return ""
	}
}

func revisionError(code, message string) error {
	var cause error
	switch code {
	case "subject_unsupported":
		cause = memory.ErrSubjectUnsupported
	case "subject_ambiguous":
		cause = memory.ErrSubjectAmbiguous
	case "subject_head_unavailable":
		cause = memory.ErrSubjectHeadUnavailable
	case "fact_changed":
		cause = memory.ErrFactChanged
	case "revision_required":
		cause = memory.ErrRevisionRequired
	case "evidence_quota":
		cause = memory.ErrEvidenceQuota
	case "read_only":
		cause = memory.ErrStoreReadOnly
	case "unsupported":
		cause = memory.ErrInspectionUnsupported
	case "deadline_exceeded":
		cause = context.DeadlineExceeded
	case "canceled":
		cause = context.Canceled
	}
	if cause == nil {
		return errors.New(message)
	}
	return fmt.Errorf("%w: %s", cause, message)
}

func (s *Server) Retire(req *RetireRequest, resp *LifecycleResponse) error {
	defer s.touch()
	store, ok := s.backend.(ConfidenceLifecycle)
	if !ok || !s.hasConfidenceCapability(ConfidenceLifecycleV1) {
		return unsupported(ConfidenceLifecycleV1)
	}
	return store.Retire(req.AgentID, req.Victims...)
}

func (s *Server) SetPinned(req *SetPinnedRequest, resp *LifecycleResponse) error {
	defer s.touch()
	store, ok := s.backend.(ConfidenceLifecycle)
	if !ok || !s.hasConfidenceCapability(ConfidenceLifecycleV1) {
		return unsupported(ConfidenceLifecycleV1)
	}
	return store.SetPinned(req.AgentID, req.Pinned, req.Victims...)
}

func (s *Server) RecallExplainWithOptions(req *RecallWithOptionsRequest, resp *RecallExplainWithOptionsResponse) error {
	defer s.touch()
	if err := req.Options.Validate(); err != nil {
		return err
	}
	recaller, ok := s.backend.(ConfidenceRecaller)
	if !ok || !s.hasConfidenceCapability(ConfidenceRecallV1) {
		return unsupported(ConfidenceRecallV1)
	}
	result, err := recaller.RecallExplainWithOptions(context.Background(), req.AgentID, req.Query, req.TopK, req.Options)
	if err != nil {
		return err
	}
	resp.Result = result
	return nil
}

func (s *Server) RecallAllWithOptions(req *RecallWithOptionsRequest, resp *RecallWithOptionsResponse) error {
	defer s.touch()
	if err := req.Options.Validate(); err != nil {
		return err
	}
	recaller, ok := s.backend.(ConfidenceRecaller)
	if !ok || !s.hasConfidenceCapability(ConfidenceRecallV1) {
		return unsupported(ConfidenceRecallV1)
	}
	result, err := recaller.RecallAllWithOptions(context.Background(), req.AgentID, req.Query, req.TopK, req.Options)
	if err != nil {
		return err
	}
	resp.Result = result
	return nil
}

// legacyRecallOptions pins legacy RPC semantics even after a future default
// promotion. These options stay private to the compatibility endpoints.
func legacyRecallOptions() memory.RecallOptions {
	zero := 0.0
	return memory.RecallOptions{ConfidenceWeight: &zero}
}

func (c *Client) requireCapability(capability string) error {
	c.capMu.Lock()
	known := c.capKnown
	c.capMu.Unlock()
	if !known {
		if err := c.Ping(); err != nil {
			return err
		}
	}
	c.capMu.Lock()
	supported := c.capabilities[capability]
	c.capMu.Unlock()
	if !supported {
		return unsupported(capability)
	}
	return nil
}

// PutWithOptionalIdentity chooses one write route after verifying daemon policy.
// A safe daemon with no identity endpoint may still accept an ordinary write
// without options. That route returns a zero Fact by contract.
func (c *Client) PutWithOptionalIdentity(ctx context.Context, agentID, text string, opts memory.WriteOptions) (memory.Fact, error) {
	var prepareErr error
	opts, prepareErr = c.prepareCitationOptions(ctx, opts)
	if prepareErr != nil {
		return memory.Fact{}, prepareErr
	}
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	if err := c.requireMaintenanceOnly(ctx); err != nil {
		return memory.Fact{}, err
	}
	withOptions, _ := c.Capability(ConfidenceWriteV1)
	withIdentity, _ := c.Capability(FactIdentityV1)
	var fact memory.Fact
	var err error
	switch {
	case withOptions:
		fact, err = c.putWithOptionsReturningFact(agentID, text, opts)
	case subjectRequested(opts):
		return memory.Fact{}, memory.ErrSubjectUnsupported
	case opts.Confidence != nil || citationRequested(opts):
		return memory.Fact{}, unsupported(ConfidenceWriteV1)
	case withIdentity:
		fact, err = c.PutReturningFact(ctx, agentID, text)
	default:
		return memory.Fact{}, c.Put(ctx, agentID, text)
	}
	if err == nil && fact.ID == "" {
		return memory.Fact{}, errors.New("rpc: write returned no committed fact identity")
	}
	return fact, err
}

func (c *Client) PutWithOptionsReturningFact(ctx context.Context, agentID, text string, opts memory.WriteOptions) (memory.Fact, error) {
	var prepareErr error
	opts, prepareErr = c.prepareCitationOptions(ctx, opts)
	if prepareErr != nil {
		return memory.Fact{}, prepareErr
	}
	if err := ctx.Err(); err != nil {
		return memory.Fact{}, err
	}
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	if err := c.requireMaintenanceOnly(ctx); err != nil {
		return memory.Fact{}, err
	}
	if err := c.requireCapability(ConfidenceWriteV1); err != nil {
		if errors.Is(err, ErrUnsupportedCapability) && opts.Confidence == nil && !citationRequested(opts) && !subjectRequested(opts) {
			return c.PutReturningFact(ctx, agentID, text)
		}
		return memory.Fact{}, err
	}
	return c.putWithOptionsReturningFact(agentID, text, opts)
}

func (c *Client) putWithOptionsReturningFact(agentID, text string, opts memory.WriteOptions) (memory.Fact, error) {
	var resp PutWithOptionsResponse
	err := c.call("PutWithOptionsReturningFact", &PutWithOptionsRequest{AgentID: agentID, Text: text, Options: opts}, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp.Fact, mapCapabilityError(err)
}

func (c *Client) PutSharedWithOptionsReturningFact(ctx context.Context, text string, opts memory.WriteOptions) (memory.Fact, error) {
	var prepareErr error
	opts, prepareErr = c.prepareCitationOptions(ctx, opts)
	if prepareErr != nil {
		return memory.Fact{}, prepareErr
	}
	if err := ctx.Err(); err != nil {
		return memory.Fact{}, err
	}
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	if err := c.requireCapability(ConfidenceSharedWriteV1); err != nil {
		return memory.Fact{}, err
	}
	var resp PutWithOptionsResponse
	err := c.call("PutSharedWithOptionsReturningFact", &PutWithOptionsRequest{Text: text, Options: opts}, &resp)
	if err == nil && resp.Error != "" {
		err = errors.New(resp.Error)
	}
	return resp.Fact, mapCapabilityError(err)
}

func (c *Client) ReviseWithOptions(ctx context.Context, agentID, target, text string, opts memory.WriteOptions) (memory.Fact, error) {
	if subjectRequested(opts) {
		return memory.Fact{}, errors.New("subject and replaces are supported only by add; revision already selects its targets")
	}
	var prepareErr error
	opts, prepareErr = c.prepareCitationOptions(ctx, opts)
	if prepareErr != nil {
		return memory.Fact{}, prepareErr
	}
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return memory.Fact{}, err
	}
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	if err := c.requireMaintenanceOnly(ctx); err != nil {
		return memory.Fact{}, err
	}
	if err := c.requireAtomicRevision(ctx); err != nil {
		return memory.Fact{}, err
	}
	if err := c.requireLineage(ctx); err != nil {
		return memory.Fact{}, err
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return memory.Fact{}, err
	}
	req := &ReviseWithOptionsRequest{Context: meta, AgentID: agentID, Target: target, Text: text, Options: opts}
	resp, err := rpcInspectionCall[ReviseWithOptionsResponse](c, ctx, "ReviseWithOptions", req, &meta, true)
	return revisionResult(resp, err)
}

func (c *Client) ReviseFactsWithOptions(ctx context.Context, agentID, text string, opts memory.WriteOptions, victims ...memory.Fact) (memory.Fact, error) {
	if subjectRequested(opts) {
		return memory.Fact{}, errors.New("subject and replaces are supported only by add; revision already selects its targets")
	}
	var prepareErr error
	opts, prepareErr = c.prepareCitationOptions(ctx, opts)
	if prepareErr != nil {
		return memory.Fact{}, prepareErr
	}
	victims = append([]memory.Fact(nil), victims...)
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return memory.Fact{}, err
	}
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	if err := c.requireMaintenanceOnly(ctx); err != nil {
		return memory.Fact{}, err
	}
	if err := c.requireAtomicRevision(ctx); err != nil {
		return memory.Fact{}, err
	}
	if err := c.requireLineage(ctx); err != nil {
		return memory.Fact{}, err
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return memory.Fact{}, err
	}
	req := &ReviseFactsWithOptionsRequest{Context: meta, AgentID: agentID, Text: text, Options: opts, Victims: victims}
	resp, err := rpcInspectionCall[ReviseWithOptionsResponse](c, ctx, "ReviseFactsWithOptions", req, &meta, true)
	return revisionResult(resp, err)
}

func revisionResult(resp ReviseWithOptionsResponse, err error) (memory.Fact, error) {
	if err != nil {
		return resp.Fact, lineageRPCError(inspectionError(mapCapabilityError(err)))
	}
	if resp.Error != "" {
		return resp.Fact, revisionError(resp.ErrorCode, resp.Error)
	}
	return resp.Fact, nil
}

func (c *Client) Retire(agentID string, victims ...memory.Fact) error {
	if err := c.requireCapability(ConfidenceLifecycleV1); err != nil {
		if errors.Is(err, ErrUnsupportedCapability) {
			return c.legacyLifecycle(agentID, nil, victims...)
		}
		return err
	}
	return mapCapabilityError(c.call("Retire", &RetireRequest{AgentID: agentID, Victims: victims}, &LifecycleResponse{}))
}

func (c *Client) SetPinned(agentID string, pinned bool, victims ...memory.Fact) error {
	if err := c.requireCapability(ConfidenceLifecycleV1); err != nil {
		if errors.Is(err, ErrUnsupportedCapability) {
			return c.legacyLifecycle(agentID, &pinned, victims...)
		}
		return err
	}
	return mapCapabilityError(c.call("SetPinned", &SetPinnedRequest{AgentID: agentID, Pinned: pinned, Victims: victims}, &LifecycleResponse{}))
}

// legacyFallback is deliberately presence-aware. Explicit zero still requests
// an effective-policy receipt that a previous daemon cannot produce.
func (c *Client) legacyFallback(opts memory.RecallOptions) bool {
	return opts.MinConfidence == nil && opts.ConfidenceWeight == nil && c.DefaultConfidenceWeight() == 0
}

func (c *Client) DefaultConfidenceWeight() float64 {
	c.capMu.Lock()
	defer c.capMu.Unlock()
	return c.defaultConfidenceWeight
}

func (c *Client) resolveClientRecallOptions(opts memory.RecallOptions) (memory.RecallOptions, bool) {
	weight := c.DefaultConfidenceWeight()
	if opts.ConfidenceWeight != nil {
		weight = *opts.ConfidenceWeight
	}
	legacyMetadata := !opts.Requested() && weight == 0
	if opts.ConfidenceWeight == nil {
		opts.ConfidenceWeight = &weight
	}
	return opts, legacyMetadata
}

func (c *Client) RecallWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallResult, error) {
	if err := ctx.Err(); err != nil {
		return memory.RecallResult{}, err
	}
	if err := opts.Validate(); err != nil {
		return memory.RecallResult{}, err
	}
	if err := c.requireCapability(ConfidenceRecallV1); err != nil {
		if errors.Is(err, ErrUnsupportedCapability) && c.legacyFallback(opts) {
			facts, feedback, err := c.RecallDetailed(ctx, agentID, query, topK)
			return memory.RecallResult{Facts: facts, Feedback: feedback}, err
		}
		return memory.RecallResult{}, err
	}
	var resp RecallWithOptionsResponse
	wireOptions, legacyMetadata := c.resolveClientRecallOptions(opts)
	err := c.call("RecallWithOptions", &RecallWithOptionsRequest{AgentID: agentID, Query: query, TopK: topK, Options: wireOptions}, &resp)
	if legacyMetadata && (resp.Result.Retrieval == nil || resp.Result.Retrieval.VectorFallback == "") {
		resp.Result.Retrieval = nil
	}
	return resp.Result, mapCapabilityError(err)
}

func (c *Client) RecallExplainWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallExplainResult, error) {
	if err := ctx.Err(); err != nil {
		return memory.RecallExplainResult{}, err
	}
	if err := opts.Validate(); err != nil {
		return memory.RecallExplainResult{}, err
	}
	if err := c.requireCapability(ConfidenceRecallV1); err != nil {
		if errors.Is(err, ErrUnsupportedCapability) && c.legacyFallback(opts) {
			receipts, err := c.RecallExplain(ctx, agentID, query, topK)
			return memory.RecallExplainResult{Receipts: receipts}, err
		}
		return memory.RecallExplainResult{}, err
	}
	var resp RecallExplainWithOptionsResponse
	wireOptions, legacyMetadata := c.resolveClientRecallOptions(opts)
	err := c.call("RecallExplainWithOptions", &RecallWithOptionsRequest{AgentID: agentID, Query: query, TopK: topK, Options: wireOptions}, &resp)
	if legacyMetadata {
		if resp.Result.Retrieval == nil || resp.Result.Retrieval.VectorFallback == "" {
			resp.Result.Retrieval = nil
		}
		for i := range resp.Result.Receipts {
			resp.Result.Receipts[i].Ranking = nil
		}
	}
	return resp.Result, mapCapabilityError(err)
}

func (c *Client) RecallAllWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallResult, error) {
	if err := ctx.Err(); err != nil {
		return memory.RecallResult{}, err
	}
	if err := opts.Validate(); err != nil {
		return memory.RecallResult{}, err
	}
	if err := c.requireCapability(ConfidenceRecallV1); err != nil {
		if errors.Is(err, ErrUnsupportedCapability) && c.legacyFallback(opts) {
			facts, err := c.RecallAll(ctx, agentID, query, topK)
			return memory.RecallResult{Facts: facts}, err
		}
		return memory.RecallResult{}, err
	}
	var resp RecallWithOptionsResponse
	wireOptions, legacyMetadata := c.resolveClientRecallOptions(opts)
	err := c.call("RecallAllWithOptions", &RecallWithOptionsRequest{AgentID: agentID, Query: query, TopK: topK, Options: wireOptions}, &resp)
	if legacyMetadata && (resp.Result.Retrieval == nil || resp.Result.Retrieval.VectorFallback == "") {
		resp.Result.Retrieval = nil
	}
	return resp.Result, mapCapabilityError(err)
}
