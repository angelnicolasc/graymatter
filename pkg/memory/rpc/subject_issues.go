package rpc

import (
	"context"
	"errors"
	"fmt"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const SubjectIssuesV1 = "subject-issues-v1"

// SubjectIssueReader is optional and does not extend the legacy store contract.
type SubjectIssueReader interface {
	SubjectIssues(context.Context, []string) (memory.SubjectIssues, error)
}

type subjectIssueCatalog interface {
	SubjectIssueReader
	SubjectIssuesByNamespace(context.Context) (map[string]memory.SubjectIssues, error)
}

type SubjectIssuesRequest struct {
	Context    InspectionContext
	Namespaces []string
	All        bool
}

type SubjectIssuesResponse struct {
	Issues      *memory.SubjectIssues           `json:",omitempty"`
	ByNamespace map[string]memory.SubjectIssues `json:",omitempty"`
}

func (s *Server) SubjectIssues(req *SubjectIssuesRequest, resp *SubjectIssuesResponse) error {
	defer s.touch()
	reader, ok := s.backend.(subjectIssueCatalog)
	if !ok {
		return ErrUnsupportedCapability
	}
	ctx, finish, err := s.beginInspection(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	if req.All {
		resp.ByNamespace, err = reader.SubjectIssuesByNamespace(ctx)
	} else {
		var issues memory.SubjectIssues
		issues, err = reader.SubjectIssues(ctx, req.Namespaces)
		if issues.Total != 0 {
			resp.Issues = &issues
		}
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}

func (c *Client) subjectIssues(ctx context.Context, request SubjectIssuesRequest) (SubjectIssuesResponse, error) {
	if err := ctx.Err(); err != nil {
		return SubjectIssuesResponse{}, err
	}
	if err := c.requireCapability(SubjectIssuesV1); err != nil {
		// Earlier peers do not have this observation. Preserve their
		// established read behavior and omit the optional diagnostic.
		if errors.Is(err, ErrUnsupportedCapability) {
			return SubjectIssuesResponse{}, nil
		}
		return SubjectIssuesResponse{}, err
	}
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return SubjectIssuesResponse{}, err
	}
	request.Context = meta
	return rpcInspectionCall[SubjectIssuesResponse](c, ctx, "SubjectIssues", &request, &meta, false)
}

func validateSubjectIssues(issues memory.SubjectIssues, allowed map[string]bool) error {
	if issues.Total < len(issues.Subjects) || len(issues.Subjects) > 3 {
		return errors.New("invalid subject diagnostics count")
	}
	for _, issue := range issues.Subjects {
		if !allowed[issue.Namespace] || issue.Name == "" {
			return fmt.Errorf("invalid subject diagnostics for namespace %q", issue.Namespace)
		}
	}
	return nil
}

func (c *Client) SubjectIssues(ctx context.Context, namespaces []string) (memory.SubjectIssues, error) {
	resp, err := c.subjectIssues(ctx, SubjectIssuesRequest{Namespaces: namespaces})
	if err != nil || resp.Issues == nil {
		return memory.SubjectIssues{}, err
	}
	allowed := make(map[string]bool, len(namespaces))
	for _, namespace := range namespaces {
		allowed[namespace] = true
	}
	if err := validateSubjectIssues(*resp.Issues, allowed); err != nil {
		return memory.SubjectIssues{}, err
	}
	return *resp.Issues, nil
}

func (c *Client) SubjectIssuesByNamespace(ctx context.Context) (map[string]memory.SubjectIssues, error) {
	resp, err := c.subjectIssues(ctx, SubjectIssuesRequest{All: true})
	if err != nil {
		return nil, err
	}
	for namespace, issues := range resp.ByNamespace {
		if err := validateSubjectIssues(issues, map[string]bool{namespace: true}); err != nil {
			return nil, err
		}
	}
	return resp.ByNamespace, nil
}
