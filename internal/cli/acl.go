package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RFC EZ §3 — `loomcycle acl`: groups, access-control lists, bindings and the
// authz questions, as a thin HTTP client to the management routes of a
// running instance, like `loomcycle operator-token`.
//
//	loomcycle acl groups   list | get --name G | effective --name G | create --name G [--description D] [--external-id X] | delete --name G
//	                       | add-member --name G --kind user|group --id ID | remove-member --name G --kind K --id ID
//	loomcycle acl policies list [--include-retired] | names | catalog | get --name L | retire --name L
//	                       | create --name L -f rules.yaml [--description D] | validate -f rules.yaml
//	loomcycle acl bindings list [--include-retired] | create --acl L --kind user|group|token|tenant [--id ID] [--expires-at RFC3339]
//	                       | delete --id B
//	loomcycle acl check    --action A --resource type[:id] [--subject S] [--owner O] [--transport T]
//	loomcycle acl explain  (the check flags)
//	loomcycle acl simulate -f draft.yaml [--subject S] [--action A --resource type[:id]]
//
// Every verb takes --target / --token (default $LOOMCYCLE_BASE_URL /
// $LOOMCYCLE_AUTH_TOKEN; the bearer is never printed) and --tenant (an
// admin's focus on another tenant). The server's JSON answer is printed to
// stdout. Exit codes match the other admin CLIs: 0 ok, 1 operational, 2 usage
// or a 4xx.

// aclRequest is one call to a management route.
type aclRequest struct {
	method, path string
	query        url.Values
	body         any
}

// aclCommon are the flags every verb takes.
type aclCommon struct {
	target, token, tenant *string
	timeout               *time.Duration
}

func aclFlags(name string, stderr io.Writer) (*flag.FlagSet, aclCommon) {
	fs := flag.NewFlagSet("acl "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs, aclCommon{
		target:  fs.String("target", defaultBaseURL(), "loomcycle base URL"),
		token:   registerTokenFlag(fs),
		tenant:  fs.String("tenant", "", "admin only: the tenant to act in (anyone else acts in its own)"),
		timeout: fs.Duration("http-timeout", 15*time.Second, "client-side HTTP request timeout"),
	}
}

// RunACL dispatches `loomcycle acl <noun> <verb> ...`.
func RunACL(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "loomcycle: error: usage: loomcycle acl <groups|policies|bindings|check|explain|simulate> ...")
		return 2
	}
	noun, rest := args[0], args[1:]
	switch noun {
	case "groups", "policies", "bindings":
		if len(rest) == 0 {
			fmt.Fprintf(stderr, "loomcycle: error: usage: loomcycle acl %s <verb> ...\n", noun)
			return 2
		}
		verb := rest[0]
		req, common, rc := aclBuild(noun, verb, rest[1:], stderr)
		if rc != 0 {
			return rc
		}
		return aclSend(common, req, stdout, stderr)
	case "check", "explain":
		return runACLQuestion(noun, rest, stdout, stderr)
	case "simulate":
		return runACLSimulate(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "loomcycle: error: unknown acl subcommand %q (want: groups / policies / bindings / check / explain / simulate)\n", noun)
		return 2
	}
}

// aclBuild parses one groups / policies / bindings verb into its request.
func aclBuild(noun, verb string, args []string, stderr io.Writer) (aclRequest, aclCommon, int) {
	fs, common := aclFlags(noun+" "+verb, stderr)
	name := fs.String("name", "", "the group or list name")
	description := fs.String("description", "", "a description")
	externalID := fs.String("external-id", "", "groups create: the group's id in an identity provider")
	kind := fs.String("kind", "", "member kind (user|group) or binding subject kind (user|group|token|tenant)")
	id := fs.String("id", "", "the member's id, the binding's subject id, or (bindings delete) the binding id")
	acl := fs.String("acl", "", "bindings create: the list to bind")
	expiresAt := fs.String("expires-at", "", "bindings create: when the binding stops applying (RFC 3339)")
	file := fs.String("f", "", "policies create / validate: a YAML or JSON file of rules (an array, or an object with rules)")
	includeRetired := fs.Bool("include-retired", false, "list: include retired rows")
	if err := fs.Parse(args); err != nil {
		return aclRequest{}, common, 2
	}
	need := func(flags ...string) bool {
		for _, f := range flags {
			if fs.Lookup(f).Value.String() == "" {
				fmt.Fprintf(stderr, "loomcycle: error: acl %s %s needs --%s\n", noun, verb, f)
				return false
			}
		}
		return true
	}
	esc := url.PathEscape
	switch noun + " " + verb {
	case "groups list":
		return aclRequest{method: http.MethodGet, path: "/v1/_groups"}, common, 0
	case "groups get":
		if !need("name") {
			return aclRequest{}, common, 2
		}
		return aclRequest{method: http.MethodGet, path: "/v1/_groups/" + esc(*name)}, common, 0
	case "groups create":
		if !need("name") {
			return aclRequest{}, common, 2
		}
		return aclRequest{method: http.MethodPost, path: "/v1/_groups",
			body: map[string]string{"name": *name, "description": *description, "external_id": *externalID}}, common, 0
	case "groups effective":
		if !need("name") {
			return aclRequest{}, common, 2
		}
		return aclRequest{method: http.MethodGet, path: "/v1/_groups/" + esc(*name) + "/effective"}, common, 0
	case "groups delete":
		if !need("name") {
			return aclRequest{}, common, 2
		}
		return aclRequest{method: http.MethodDelete, path: "/v1/_groups/" + esc(*name)}, common, 0
	case "groups add-member":
		if !need("name", "kind", "id") {
			return aclRequest{}, common, 2
		}
		return aclRequest{method: http.MethodPost, path: "/v1/_groups/" + esc(*name) + "/members",
			body: map[string]string{"kind": *kind, "id": *id}}, common, 0
	case "groups remove-member":
		if !need("name", "kind", "id") {
			return aclRequest{}, common, 2
		}
		return aclRequest{method: http.MethodDelete, path: "/v1/_groups/" + esc(*name) + "/members/" + esc(*kind) + "/" + esc(*id)}, common, 0

	case "policies list":
		body := map[string]any{"op": "list"}
		if *includeRetired {
			body["include_retired"] = true
		}
		return aclRequest{method: http.MethodPost, path: "/v1/_acldef", body: body}, common, 0
	case "policies names":
		return aclRequest{method: http.MethodGet, path: "/v1/_acldef/names"}, common, 0
	case "policies catalog":
		return aclRequest{method: http.MethodPost, path: "/v1/_acldef", body: map[string]any{"op": "catalog"}}, common, 0
	case "policies get", "policies retire":
		if !need("name") {
			return aclRequest{}, common, 2
		}
		return aclRequest{method: http.MethodPost, path: "/v1/_acldef", body: map[string]any{"op": verb, "name": *name}}, common, 0
	case "policies create", "policies validate":
		flags := []string{"f"}
		if verb == "create" {
			flags = append(flags, "name")
		}
		if !need(flags...) {
			return aclRequest{}, common, 2
		}
		rules, err := readRulesFile(*file)
		if err != nil {
			fmt.Fprintf(stderr, "loomcycle: error: %v\n", err)
			return aclRequest{}, common, 2
		}
		body := map[string]any{"op": verb, "rules": rules}
		if verb == "create" {
			body["name"] = *name
			if *description != "" {
				body["description"] = *description
			}
		}
		return aclRequest{method: http.MethodPost, path: "/v1/_acldef", body: body}, common, 0

	case "bindings list":
		req := aclRequest{method: http.MethodGet, path: "/v1/_aclbinding"}
		if *includeRetired {
			req.query = url.Values{"include_retired": {"1"}}
		}
		return req, common, 0
	case "bindings create":
		if !need("acl", "kind") {
			return aclRequest{}, common, 2
		}
		body := map[string]any{"acl": *acl, "subject_kind": *kind}
		if *id != "" {
			body["subject_id"] = *id
		}
		if *expiresAt != "" {
			if _, err := time.Parse(time.RFC3339, *expiresAt); err != nil {
				fmt.Fprintf(stderr, "loomcycle: error: --expires-at must be RFC 3339 (e.g. 2026-12-31T00:00:00Z): %v\n", err)
				return aclRequest{}, common, 2
			}
			body["expires_at"] = *expiresAt
		}
		return aclRequest{method: http.MethodPost, path: "/v1/_aclbinding", body: body}, common, 0
	case "bindings delete":
		if !need("id") {
			return aclRequest{}, common, 2
		}
		return aclRequest{method: http.MethodDelete, path: "/v1/_aclbinding/" + esc(*id)}, common, 0
	}
	fmt.Fprintf(stderr, "loomcycle: error: unknown verb %q for acl %s\n", verb, noun)
	return aclRequest{}, common, 2
}

// questionFlags are the flags of one authz question.
type questionFlags struct {
	action, resource, subject, owner, transport *string
}

func registerQuestionFlags(fs *flag.FlagSet) questionFlags {
	return questionFlags{
		action:    fs.String("action", "", "the verb, e.g. run, invoke, read"),
		resource:  fs.String("resource", "", "type[:id], e.g. tool:Bash or agent:chat (omit the id for any instance)"),
		subject:   fs.String("subject", "", "whose rules to ask (default: the token's own subject; another needs a tenant operator)"),
		owner:     fs.String("owner", "", "the resource's owner, for an owner_is_self condition"),
		transport: fs.String("transport", "", "the transport to ask as (http, grpc, mcp, webui, …)"),
	}
}

// evaluation builds the AuthZEN request body; ok is false when neither
// --action nor --resource was given.
func (q questionFlags) evaluation() (map[string]any, bool, error) {
	if *q.action == "" && *q.resource == "" {
		return nil, false, nil
	}
	if *q.action == "" || *q.resource == "" {
		return nil, false, fmt.Errorf("--action and --resource go together")
	}
	typ, id, _ := strings.Cut(*q.resource, ":")
	res := map[string]any{"type": typ, "id": id}
	if *q.owner != "" {
		res["owner"] = *q.owner
	}
	body := map[string]any{"action": map[string]string{"name": *q.action}, "resource": res}
	if *q.transport != "" {
		body["context"] = map[string]string{"transport": *q.transport}
	}
	return body, true, nil
}

func runACLQuestion(verb string, args []string, stdout, stderr io.Writer) int {
	fs, common := aclFlags(verb, stderr)
	q := registerQuestionFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	body, ok, err := q.evaluation()
	if err != nil || !ok {
		fmt.Fprintf(stderr, "loomcycle: error: acl %s needs --action and --resource\n", verb)
		return 2
	}
	if *q.subject != "" {
		body["subject"] = map[string]string{"id": *q.subject}
	}
	path := "/v1/_authz/evaluation"
	if verb == "explain" {
		path = "/v1/_authz/explain"
	}
	return aclSend(common, aclRequest{method: http.MethodPost, path: path, body: body}, stdout, stderr)
}

// runACLSimulate evaluates against a draft read from a file: an object with
// acls, bindings and requests — the body of POST /v1/_authz/simulate, in
// YAML or JSON. --action/--resource add one more request.
func runACLSimulate(args []string, stdout, stderr io.Writer) int {
	fs, common := aclFlags("simulate", stderr)
	file := fs.String("f", "", "the draft: a YAML or JSON file with acls, bindings and requests")
	q := registerQuestionFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" {
		fmt.Fprintln(stderr, "loomcycle: error: acl simulate needs -f draft.yaml")
		return 2
	}
	var draft map[string]any
	if err := readYAMLOrJSON(*file, &draft); err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: %v\n", err)
		return 2
	}
	if draft == nil {
		draft = map[string]any{}
	}
	extra, ok, err := q.evaluation()
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: %v\n", err)
		return 2
	}
	if ok {
		reqs, _ := draft["requests"].([]any)
		draft["requests"] = append(reqs, extra)
	}
	if *q.subject != "" {
		draft["subject"] = map[string]string{"id": *q.subject}
	}
	return aclSend(common, aclRequest{method: http.MethodPost, path: "/v1/_authz/simulate", body: draft}, stdout, stderr)
}

// readRulesFile reads a rule list: an array of rules, or an object whose
// rules field is one.
func readRulesFile(path string) (any, error) {
	var v any
	if err := readYAMLOrJSON(path, &v); err != nil {
		return nil, err
	}
	if m, ok := v.(map[string]any); ok {
		rules, ok := m["rules"]
		if !ok {
			return nil, fmt.Errorf("%s: an object needs a rules field", path)
		}
		return rules, nil
	}
	if _, ok := v.([]any); !ok {
		return nil, fmt.Errorf("%s: want an array of rules, or an object with rules", path)
	}
	return v, nil
}

// readYAMLOrJSON decodes a file as YAML, which also reads JSON.
func readYAMLOrJSON(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}

// aclSend makes the call and prints the server's answer.
func aclSend(common aclCommon, req aclRequest, stdout, stderr io.Writer) int {
	q := req.query
	if *common.tenant != "" {
		if q == nil {
			q = url.Values{}
		}
		q.Set("tenant", *common.tenant)
	}
	target := strings.TrimRight(*common.target, "/") + req.path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var body []byte
	if req.body != nil {
		var err error
		if body, err = json.Marshal(req.body); err != nil {
			return failOp(stderr, "encode the request: %v", err)
		}
	}
	rc, resp, err := doAdminRequest(req.method, target, *common.token, body, *common.timeout)
	if err != nil {
		return failOp(stderr, "%s %s: %v", req.method, target, err)
	}
	if rc != 0 {
		return failPrintingBody(stderr, target, resp, rc)
	}
	_, _ = stdout.Write(append(resp, '\n'))
	return 0
}
