// Package authz is the access-control decision engine (RFC EY): user groups,
// ACL rule lists, and one evaluation that answers "may this principal do this
// action on this resource".
//
// It is a leaf package on purpose. It knows the closed catalog of resource
// types and verbs (catalog.go), how a selector names instances (selector.go),
// the shape of a rule list and a binding (rule.go), the closed condition set
// (condition.go), and how to compile a principal's applicable rules into an
// Effective set that decides in microseconds (engine.go). It knows nothing
// about tokens, stores, routes or tools: the surfaces hand it a Subject, an
// action, a Resource and a Context, and read back a Decision.
//
// Evaluation (RFC EY §3.4): the tenant floor is checked before any rule;
// then every matching deny wins over every matching allow; no match is
// NoMatch, which the caller turns into deny (enforce mode) or into today's
// scope-gate answer (compat mode). The result does not depend on the order
// of rules or bindings, adding a deny or removing an allow never widens, and
// no rule in one tenant can affect a decision in another; engine_test.go
// holds those three properties.
package authz
