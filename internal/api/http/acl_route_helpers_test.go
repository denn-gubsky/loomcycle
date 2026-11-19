package http

// The route table's answers by method and path, as the tests written against
// the hand-kept gates ask them (RFC EZ P6 moved those gates into
// authz.HTTPRoute).

func requiredScopeFor(method, path string) string { return routeAt(method, path).Scope }

func runPlaneRoute(method, path string) bool { return routeAt(method, path).RunPlane }
