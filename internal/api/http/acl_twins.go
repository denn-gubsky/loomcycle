package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC EZ §3 — the op-dispatched twins of the access-control management
// routes, served to the gRPC GroupDef / AclDef / AclBinding / Authz RPCs and
// the MCP meta-tools of the same names. The input is the HTTP route's JSON
// body plus "op" and an optional "tenant" (the admin's focus, the route's
// ?tenant=); the result text is the route's JSON body. Every op calls the
// operation the route calls, which holds every gate (aclEnter): the twins add
// a decoder, never a rule.
//
// It is not a tools.Tool and no run's registry can hold it, and aclEnter
// refuses a ctx that belongs to a run, so an agent cannot reach it however a
// transport is wired.

// GroupDef serves the groupdef family.
func (s *Server) GroupDef(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return aclToolResult(s.aclTwin(ctx, connector.ACLFamilyGroupDef, input)), nil
}

// AclDef serves the acldef family.
func (s *Server) AclDef(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return aclToolResult(s.aclTwin(ctx, connector.ACLFamilyAclDef, input)), nil
}

// AclBinding serves the aclbinding family.
func (s *Server) AclBinding(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return aclToolResult(s.aclTwin(ctx, connector.ACLFamilyAclBinding, input)), nil
}

// Authz serves the authz family.
func (s *Server) Authz(ctx context.Context, input json.RawMessage) (connector.ToolResult, error) {
	return aclToolResult(s.aclTwin(ctx, connector.ACLFamilyAuthz, input)), nil
}

// aclTwinHead is the two fields every twin input carries besides the route's
// body.
type aclTwinHead struct {
	Op     string `json:"op"`
	Tenant string `json:"tenant,omitempty"`
}

// aclTwin decodes one twin call and runs the operation its op names. The
// class gate runs before the input is even read, so a refused caller learns
// nothing from a decode error; the operation runs it again with its own
// type-level question.
func (s *Server) aclTwin(ctx context.Context, family string, input json.RawMessage) aclReply {
	if r := s.aclEnter(ctx, family != connector.ACLFamilyAuthz, nil); r != nil {
		return *r
	}
	if len(input) > aclBodyCap {
		return aclFail(http.StatusBadRequest, "invalid_body", "invalid request body: request body too large")
	}
	var head aclTwinHead
	if err := json.Unmarshal(input, &head); err != nil {
		return aclFail(http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
	}
	// acldef asks its per-op question before it looks at the op, so an op it
	// does not know is asked author and then refused (aclDefOp): the twin
	// hands every op straight through, as the route does.
	if family == connector.ACLFamilyAclDef {
		var in struct {
			Tenant string `json:"tenant,omitempty"`
			aclDefRequest
		}
		if r := aclDecodeStrict(input, &in); r != nil {
			return *r
		}
		return s.aclDefOp(ctx, in.Tenant, in.aclDefRequest)
	}
	ops := connector.ACLManagementOps(family)
	if !slices.Contains(ops, head.Op) {
		return aclFail(http.StatusBadRequest, "unknown_op", fmt.Sprintf("op must be one of %s", strings.Join(ops, ", ")))
	}
	switch family + "." + head.Op {
	case "groupdef.list":
		var in aclTwinHead
		if r := aclDecodeStrict(input, &in); r != nil {
			return *r
		}
		return s.aclGroupList(ctx, in.Tenant)
	case "groupdef.get", "groupdef.effective", "groupdef.delete":
		var in struct {
			aclTwinHead
			Name string `json:"name"`
		}
		if r := aclDecodeStrict(input, &in); r != nil {
			return *r
		}
		if r := aclRequire("name", in.Name); r != nil {
			return *r
		}
		switch head.Op {
		case "get":
			return s.aclGroupGet(ctx, in.Tenant, in.Name)
		case "effective":
			return s.aclGroupEffective(ctx, in.Tenant, in.Name)
		}
		return s.aclGroupDelete(ctx, in.Tenant, in.Name)
	case "groupdef.create":
		var in struct {
			aclTwinHead
			groupRequest
		}
		if r := aclDecodeStrict(input, &in); r != nil {
			return *r
		}
		return s.aclGroupPut(ctx, in.Tenant, in.groupRequest)
	case "groupdef.add_member":
		var in struct {
			aclTwinHead
			Name string `json:"name"`
			groupMemberRequest
		}
		if r := aclDecodeStrict(input, &in); r != nil {
			return *r
		}
		if r := aclRequire("name", in.Name); r != nil {
			return *r
		}
		return s.aclGroupAddMember(ctx, in.Tenant, in.Name, in.groupMemberRequest)
	case "groupdef.remove_member":
		var in struct {
			aclTwinHead
			Name string `json:"name"`
			Kind string `json:"kind"`
			ID   string `json:"id"`
		}
		if r := aclDecodeStrict(input, &in); r != nil {
			return *r
		}
		if r := aclRequire("name, kind and id", in.Name, in.Kind, in.ID); r != nil {
			return *r
		}
		return s.aclGroupRemoveMember(ctx, in.Tenant, in.Name, in.Kind, in.ID)

	case "aclbinding.list":
		var in struct {
			aclTwinHead
			IncludeRetired bool `json:"include_retired,omitempty"`
		}
		if r := aclDecodeStrict(input, &in); r != nil {
			return *r
		}
		return s.aclBindingList(ctx, in.Tenant, in.IncludeRetired)
	case "aclbinding.create":
		var in struct {
			aclTwinHead
			bindingRequest
		}
		if r := aclDecodeStrict(input, &in); r != nil {
			return *r
		}
		return s.aclBindingCreate(ctx, in.Tenant, in.bindingRequest)
	case "aclbinding.delete":
		var in struct {
			aclTwinHead
			ID string `json:"id"`
		}
		if r := aclDecodeStrict(input, &in); r != nil {
			return *r
		}
		if r := aclRequire("id", in.ID); r != nil {
			return *r
		}
		return s.aclBindingRetire(ctx, in.Tenant, in.ID)

	case "authz.evaluation", "authz.explain":
		var in struct {
			aclTwinHead
			authzEvaluationRequest
		}
		if r := aclDecodeStrict(input, &in); r != nil {
			return *r
		}
		if head.Op == "evaluation" {
			return s.authzEvaluation(ctx, in.Tenant, in.authzEvaluationRequest)
		}
		return s.authzExplain(ctx, in.Tenant, in.authzEvaluationRequest)
	case "authz.evaluations":
		var in struct {
			aclTwinHead
			authzEvaluationsRequest
		}
		if r := aclDecodeStrict(input, &in); r != nil {
			return *r
		}
		return s.authzEvaluations(ctx, in.Tenant, in.authzEvaluationsRequest)
	case "authz.simulate":
		var in struct {
			aclTwinHead
			authzSimulateRequest
		}
		if r := aclDecodeStrict(input, &in); r != nil {
			return *r
		}
		return s.authzSimulate(ctx, in.Tenant, in.authzSimulateRequest)
	}
	// An op in the table with no case above: a table edit that forgot the
	// dispatcher. TestACLTwins_DispatchEveryOpInTheTable holds this unreachable.
	return aclFail(http.StatusInternalServerError, "unknown_op", fmt.Sprintf("%s op %q is listed but not dispatched", family, head.Op))
}

// aclDecodeStrict decodes a twin's input into the op's own shape, refusing a
// field the op does not take, as the HTTP route refuses one its body does
// not (decodeACLBody).
func aclDecodeStrict(input json.RawMessage, into any) *aclReply {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		r := aclFail(http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return &r
	}
	return nil
}

// aclRequire refuses an empty value the HTTP route takes from its path.
func aclRequire(what string, values ...string) *aclReply {
	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			r := aclFail(http.StatusBadRequest, "missing_field", what+" required")
			return &r
		}
	}
	return nil
}

// aclToolResult renders a reply as a twin's result: the body the route
// would write as the text, and for a refusal the failure's category.
func aclToolResult(r aclReply) connector.ToolResult {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r.body); err != nil {
		return connector.ToolResult{Text: `{"code":"encode_failed","error":"the reply could not be encoded"}`, IsError: true,
			ErrorInfo: &tools.ErrorInfo{Category: tools.CategoryTransient, Description: "The reply could not be encoded; retry."}}
	}
	text := strings.TrimSuffix(buf.String(), "\n")
	if r.status < http.StatusBadRequest {
		return connector.ToolResult{Text: text}
	}
	info := &tools.ErrorInfo{Category: tools.CategoryValidation, Description: aclReplyError(r)}
	switch {
	case r.status == http.StatusUnauthorized || r.status == http.StatusForbidden:
		info.Category = tools.CategoryPermission
	case r.status == http.StatusNotFound || r.status == http.StatusConflict:
		info.Category = tools.CategoryBusiness
	case r.status >= http.StatusInternalServerError:
		info.Category, info.Retryable = tools.CategoryTransient, true
	}
	return connector.ToolResult{Text: text, IsError: true, ErrorInfo: info}
}

// aclReplyError is a refusal's message.
func aclReplyError(r aclReply) string {
	switch b := r.body.(type) {
	case aclErrorBody:
		return b.Error
	case aclScopeBody:
		return b.Error
	case aclDeniedReply:
		return b.Error
	}
	return http.StatusText(r.status)
}
