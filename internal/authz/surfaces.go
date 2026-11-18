package authz

// AnyInstance is the resource name of a TYPE-LEVEL question: "may the
// principal do this action on any instance of the type". A selector matches
// it only when it covers every instance (`agent:*`, `memory:*`,
// `document:*`), so a rule about some instances (`deny run agent:admin/*`)
// is neither an allow nor a deny at the type level — the instance check at
// the handler decides. The transport gates ask the type-level question
// when the request has not been read yet (RFC EZ §1.1).
const AnyInstance = "*"

// Surface is what one transport entry point asks the engine: the action
// and the resource, type-level (Name == AnyInstance) or the instance the
// path or method names.
type Surface struct {
	Action string
	Type   string
	Name   string
}

func typeLevel(action, typ string) Surface {
	return Surface{Action: action, Type: typ, Name: AnyInstance}
}

func instance(action, typ, name string) Surface {
	if name == "" {
		name = AnyInstance
	}
	return Surface{Action: action, Type: typ, Name: name}
}

// Resource returns the surface as a Resource in tenant.
func (s Surface) Resource(tenant string) Resource {
	return Resource{Type: s.Type, Name: s.Name, TenantID: tenant}
}

// HTTPSurface is the ACL question of the route a request for method and path
// is served by (its row in httpRoutes). The second result is false for a route
// no ACL governs, and for a path no route serves.
//
// A route whose body carries the op (a `_<family>def` POST dispatches
// create/fork/get/list/retire on `op`) asks the WEAKEST verb its handler may
// need, `read def:<fam>/*`; the handler asks the per-op instance question.
func HTTPSurface(method, path string) (Surface, bool) {
	rt, ok := HTTPRoute(method, path)
	if !ok {
		return Surface{}, false
	}
	return rt.Question(method, path)
}

func channelVerb(op string) string {
	switch op {
	case "publish":
		return "publish"
	case "subscribe", "peek":
		return "subscribe"
	case "ack":
		return "ack"
	case "await":
		return "await"
	case "broadcast":
		return "broadcast"
	}
	return "manage"
}

// MCPToolSurface is the question the loomcycle-as-MCP-server per-tool gate
// asks for one of its meta-tools.
func MCPToolSurface(tool string) Surface { return instance("invoke", "mcp-tool", tool) }
