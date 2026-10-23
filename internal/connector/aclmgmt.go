package connector

// The access-control management families (RFC EZ §3) and their ops: the one
// table the shared implementation dispatches on, the MCP tool schemas
// enumerate, the CLI targets and the parity tests check every transport
// against. Each family is a gRPC RPC (GroupDef, AclDef, AclBinding, Authz)
// and an MCP meta-tool of the family's name; each op is one HTTP route (or,
// for acldef, one op of POST /v1/_acldef).
const (
	ACLFamilyGroupDef   = "groupdef"
	ACLFamilyAclDef     = "acldef"
	ACLFamilyAclBinding = "aclbinding"
	ACLFamilyAuthz      = "authz"
)

// ACLManagementFamilies returns the four families, in a stable order.
func ACLManagementFamilies() []string {
	return []string{ACLFamilyGroupDef, ACLFamilyAclDef, ACLFamilyAclBinding, ACLFamilyAuthz}
}

// ACLManagementOps returns a family's ops, or nil for an unknown family. A
// fresh slice each call, so no caller can change the table.
func ACLManagementOps(family string) []string {
	switch family {
	case ACLFamilyGroupDef:
		return []string{"list", "get", "create", "delete", "add_member", "remove_member"}
	case ACLFamilyAclDef:
		// create, update and put are one upsert, as on POST /v1/_acldef;
		// names is GET /v1/_acldef/names.
		return []string{"create", "update", "put", "get", "list", "retire", "validate", "catalog", "names"}
	case ACLFamilyAclBinding:
		return []string{"list", "create", "delete"}
	case ACLFamilyAuthz:
		return []string{"evaluation", "evaluations", "explain", "simulate"}
	}
	return nil
}
