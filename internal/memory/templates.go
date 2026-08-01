// templates.go — embedded Markdown templates for lazily-provisioned memory-tier
// Documents (RFC BL P1). Kept beside inject.go so the memory package owns both
// the {{memory:...}} composition and the seed content it composes.
package memory

import _ "embed"

// UserRootPath is the canonical Path-tree location, in the USER scope, of the
// operator-authored user-root Document that composes into {{memory:user_info}}.
// A single well-known path so the injector can look it up (and lazily provision
// it) without a per-run naming choice.
const UserRootPath = "/memory/user_root"

// UserRootTitle is the document title provisioned from the template — it MUST
// match the template's first heading (import_md makes the first heading the
// root chunk / document title).
const UserRootTitle = "User Profile"

//go:embed templates/user_root.md
var userRootTemplate string

// UserRootTemplate returns the import_md-shaped Markdown used to seed an empty
// user-root Document on first reference. It is a compile-time constant, so
// provisioning is deterministic (no random content).
func UserRootTemplate() string { return userRootTemplate }

// TenantRootPath is the canonical Path-tree location, in the TENANT scope, of the
// operator-authored deployment-context Document that composes into
// {{memory:tenant_info}}.
//
// Tenant scope, not user: this is what every agent in the tenant shares, and the
// per-person half already exists at UserRootPath. The two are separate documents
// rather than sections of one because they have different audiences and different
// blast radius — anything here is read by every user of the tenant.
const TenantRootPath = "/memory/tenant_root"

// TenantRootTitle MUST match the template's first heading (import_md makes the
// first heading the root chunk / document title).
const TenantRootTitle = "Deployment Context"

//go:embed templates/tenant_root.md
var tenantRootTemplate string

// TenantRootTemplate returns the import_md-shaped Markdown used to seed an empty
// tenant-root Document on first reference. Compile-time constant, so provisioning
// is deterministic.
func TenantRootTemplate() string { return tenantRootTemplate }
