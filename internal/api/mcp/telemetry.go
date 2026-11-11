package mcp

import (
	"context"
	"encoding/json"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// handleTelemetry serves the `telemetry` tool: a tenant's own trace export,
// op=get | set | delete | test, delegating to the shared connector so this
// transport cannot differ from HTTP or gRPC in who may call it, the policy
// rules or what is audited.
func handleTelemetry(ctx context.Context, env *handlerEnv, args json.RawMessage) (*loommcp.CallToolResult, error) {
	var p struct {
		Op                string   `json:"op"`
		Tenant            string   `json:"tenant"`
		Endpoint          string   `json:"endpoint"`
		Protocol          string   `json:"protocol"`
		HeadersCredential string   `json:"headers_credential"`
		Signals           []string `json:"signals"`
		SamplerRatio      *float64 `json:"sampler_ratio"`
		ContentCapture    string   `json:"content_capture"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return toolErr("invalid telemetry arguments: " + err.Error()), nil
		}
	}
	if p.Op == "" {
		p.Op = "get"
	}
	req := connector.TenantTelemetryRequest{Op: p.Op, Tenant: p.Tenant}
	if p.Op == "set" {
		req.Policy = &connector.TenantTelemetryPolicyInput{
			Endpoint: p.Endpoint, Protocol: p.Protocol, HeadersCredential: p.HeadersCredential,
			Signals: p.Signals, SamplerRatio: p.SamplerRatio, ContentCapture: p.ContentCapture,
		}
	}
	res, err := env.connector.TenantTelemetry(ctx, req)
	if err != nil {
		return toolErrFrom("telemetry "+p.Op, err), nil
	}
	return toolResultJSON(res), nil
}

// telemetryToolDescriptor is the `telemetry` tool.
func telemetryToolDescriptor() loommcp.ToolDescriptor {
	return loommcp.ToolDescriptor{
		Name:        "telemetry",
		Description: telemetryToolDescription,
		InputSchema: rawJSON(telemetryToolSchema),
	}
}

// telemetryToolDescription is the `telemetry` tool's description.
const telemetryToolDescription = "Send this tenant's own traces, and its own series of the deployment's metrics, to an OpenTelemetry (OTLP) collector of the tenant's, beside the deployment's own telemetry. " +
	"op=get (default) returns the tenant's export policy and this server's export status (spans exported and dropped, failed exports, the last success, the last error). " +
	"op=set stores the policy: endpoint (the collector's OTLP base URL, https), protocol (http/protobuf, the default, or grpc), headers_credential (the name of a tenant credential whose value is the header block, name=value,name=value, for example authorization=Basic ...,x-scope-orgid=...; the credential must be restricted by its allowed_urls to the endpoint), signals (traces, metrics or both; traces by default), sampler_ratio (0 to 1). A set replaces the whole policy. " +
	"op=test sends one test span (and, with metrics, one test metric) to the stored endpoint and answers whether it arrived, with the collector's error when not. op=delete stops the export. " +
	"Only spans of this tenant's own work are sent, without the deployment's own resource attributes or error text; spans of no-trace runs are never sent. Metrics are this tenant's own series only, pushed every minute. " +
	"For an admin or a tenant operator only. The tenant is your own unless you are an admin and pass another. Off unless the deployment's operator turned tenant export on (the call then fails with feature_disabled). " +
	"Do NOT put a token or password in endpoint, and do NOT pass header values here: store them as a tenant credential (credentialdef) and name it in headers_credential. " +
	"Do NOT use it to read run transcripts or the deployment's own traces."

const telemetryToolSchema = `{
	"type": "object",
	"properties": {
		"op":                 {"type": "string", "enum": ["get", "set", "delete", "test"], "description": "get (default), set, delete or test."},
		"tenant":             {"type": "string", "description": "Admin only: whose export. Ignored for a tenant operator's own; naming another tenant is refused."},
		"endpoint":           {"type": "string", "description": "set: the OTLP base URL of the collector, https (for example https://otlp-gateway.example.net/otlp). No user, password or query."},
		"protocol":           {"type": "string", "enum": ["http/protobuf", "grpc"], "description": "set: the OTLP protocol; http/protobuf by default."},
		"headers_credential": {"type": "string", "description": "set: the name of a tenant credential holding the header block (name=value,name=value), restricted by its allowed_urls to the endpoint. Omit for a collector that takes no headers."},
		"signals":            {"type": "array", "items": {"type": "string", "enum": ["traces", "metrics"]}, "description": "set: what to send: traces, metrics (this tenant's own series, pushed every minute) or both; traces by default."},
		"sampler_ratio":      {"type": "number", "minimum": 0, "maximum": 1, "description": "set: the share of traces to send, 0 to 1 (at most the deployment's cap); 1 by default."},
		"content_capture":    {"type": "string", "enum": ["off"], "description": "set: off. Prompt and response content is never sent."}
	}
}`
