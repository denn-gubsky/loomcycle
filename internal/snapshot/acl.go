package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The acl section (RFC EY §6): every tenant's rule lists, groups, memberships
// and bindings.
//
// Restore follows the rule every section follows: a row the target already
// holds on the same key stands — (tenant, name) for a list or a group, the
// whole edge for a membership, the id for a binding — so a re-restore writes
// nothing and an old snapshot cannot roll back a list the target has since
// tightened. Rows the target lacks are added. When a held list's rules differ
// from the snapshot's the operator is told, as for a definition conflict.
//
// The store re-validates every row against this build (the rule list against
// its catalog, the group nesting, the binding shape), so a row no author here
// could have written is skipped with a warning. Each insert bumps its tenant's
// acl_version, and the tenants touched go to the caller's post-restore refresh
// (RestoreRefresh.ACLTenants), which drops every replica's cached sets for
// them over the loomcycle.acl_changed topic.

// captureACL reads every tenant's access-control rows.
func captureACL(ctx context.Context, s store.Store, out *ACLSection) error {
	out.Version = SectionVersion
	snap, err := s.SnapshotReadAcl(ctx)
	if err != nil {
		return fmt.Errorf("snapshot acl: %w", err)
	}
	out.Lists = make([]ACLListEntry, 0, len(snap.Defs))
	for _, r := range snap.Defs {
		out.Lists = append(out.Lists, ACLListEntry{
			TenantID:    r.TenantID,
			Name:        r.Name,
			Version:     r.Version,
			Rules:       r.Rules,
			Description: r.Description,
			// sqlite reads an instant back in the host's zone; the envelope
			// is portable, so it carries UTC.
			CreatedAt: r.CreatedAt.UTC(),
			UpdatedAt: r.UpdatedAt.UTC(),
			CreatedBy: r.CreatedBy,
			RetiredAt: utcPtr(r.RetiredAt),
		})
	}
	out.Groups = make([]ACLGroupEntry, 0, len(snap.Groups))
	for _, r := range snap.Groups {
		out.Groups = append(out.Groups, ACLGroupEntry{
			TenantID:    r.TenantID,
			Name:        r.Name,
			Description: r.Description,
			ExternalID:  r.ExternalID,
			CreatedAt:   r.CreatedAt.UTC(),
			UpdatedAt:   r.UpdatedAt.UTC(),
			CreatedBy:   r.CreatedBy,
		})
	}
	out.Members = make([]ACLMemberEntry, 0, len(snap.Members))
	for _, r := range snap.Members {
		out.Members = append(out.Members, ACLMemberEntry{
			TenantID:   r.TenantID,
			Group:      r.GroupName,
			MemberKind: r.MemberKind,
			MemberID:   r.MemberID,
			AddedAt:    r.AddedAt.UTC(),
			AddedBy:    r.AddedBy,
		})
	}
	out.Bindings = make([]ACLBindingEntry, 0, len(snap.Bindings))
	for _, r := range snap.Bindings {
		out.Bindings = append(out.Bindings, ACLBindingEntry{
			BindingID:   r.BindingID,
			TenantID:    r.TenantID,
			ACL:         r.ACLName,
			SubjectKind: r.SubjectKind,
			SubjectID:   r.SubjectID,
			ExpiresAt:   utcPtr(r.ExpiresAt),
			CreatedAt:   r.CreatedAt.UTC(),
			CreatedBy:   r.CreatedBy,
			RetiredAt:   utcPtr(r.RetiredAt),
		})
	}
	return nil
}

// utcPtr is t in UTC, or nil when unset.
func utcPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// orZero is *p, or the zero time.
func orZero(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

// restoreACL writes the section in dependency order: groups, then the edges
// between them and their members, then the lists, then the bindings that name
// lists, groups and subjects. A binding to a list or group the restore did
// not bring back is still written — it matches nothing until one exists,
// exactly as the management surface allows.
func restoreACL(ctx context.Context, s store.Store, sec *ACLSection, result *RestoreResult) {
	touched := map[string]bool{}
	for _, e := range sec.Groups {
		inserted, err := s.SnapshotRestoreGroup(ctx, store.GroupRow{
			TenantID:    e.TenantID,
			Name:        e.Name,
			Description: e.Description,
			ExternalID:  e.ExternalID,
			CreatedAt:   e.CreatedAt,
			UpdatedAt:   e.UpdatedAt,
			CreatedBy:   e.CreatedBy,
		})
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("acl group %s: %v; skipped", qualifiedName(e.TenantID, e.Name), err))
			continue
		}
		if inserted {
			result.ACLGroupsRestored++
			touched[e.TenantID] = true
		}
	}
	for _, e := range sec.Members {
		inserted, err := s.SnapshotRestoreGroupMember(ctx, store.GroupMemberRow{
			TenantID:   e.TenantID,
			GroupName:  e.Group,
			MemberKind: e.MemberKind,
			MemberID:   e.MemberID,
			AddedAt:    e.AddedAt,
			AddedBy:    e.AddedBy,
		})
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("acl group member %s %s:%s: %v; skipped",
				qualifiedName(e.TenantID, e.Group), e.MemberKind, e.MemberID, err))
			continue
		}
		if inserted {
			result.ACLMembersRestored++
			touched[e.TenantID] = true
		}
	}
	for _, e := range sec.Lists {
		inserted, err := s.SnapshotRestoreAclDef(ctx, store.AclDefRow{
			TenantID:    e.TenantID,
			Name:        e.Name,
			Version:     e.Version,
			Rules:       e.Rules,
			Description: e.Description,
			CreatedAt:   e.CreatedAt,
			UpdatedAt:   e.UpdatedAt,
			CreatedBy:   e.CreatedBy,
			RetiredAt:   orZero(e.RetiredAt),
		})
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("acl list %s: %v; skipped", qualifiedName(e.TenantID, e.Name), err))
			continue
		}
		if inserted {
			result.ACLListsRestored++
			touched[e.TenantID] = true
			continue
		}
		if w := heldListDiffers(ctx, s, e); w != "" {
			result.Warnings = append(result.Warnings, w)
		}
	}
	for _, e := range sec.Bindings {
		inserted, err := s.SnapshotRestoreAclBinding(ctx, store.AclBindingRow{
			BindingID:   e.BindingID,
			TenantID:    e.TenantID,
			ACLName:     e.ACL,
			SubjectKind: e.SubjectKind,
			SubjectID:   e.SubjectID,
			ExpiresAt:   orZero(e.ExpiresAt),
			CreatedAt:   e.CreatedAt,
			CreatedBy:   e.CreatedBy,
			RetiredAt:   orZero(e.RetiredAt),
		})
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("acl binding %s (%s): %v; skipped",
				e.BindingID, qualifiedName(e.TenantID, e.ACL), err))
			continue
		}
		if inserted {
			result.ACLBindingsRestored++
			touched[e.TenantID] = true
		}
	}
	for t := range touched {
		result.Refresh.ACLTenants = append(result.Refresh.ACLTenants, t)
	}
	sort.Strings(result.Refresh.ACLTenants)
}

// heldListDiffers names a list the restore left alone whose rules or
// retirement differ from the snapshot's, so the operator knows the live list
// is not the one the source enforced. "" when they agree or the live list
// cannot be read (the restore has nothing more to say then).
func heldListDiffers(ctx context.Context, s store.Store, e ACLListEntry) string {
	live, err := s.AclDefGet(ctx, e.TenantID, e.Name)
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, e.Rules); err != nil {
		return ""
	}
	sum := sha256.Sum256(buf.Bytes())
	sameRules := hex.EncodeToString(sum[:]) == live.ContentSHA256
	if sameRules && (e.RetiredAt != nil) == live.Retired() {
		return ""
	}
	return fmt.Sprintf("acl list %s: this instance holds a different list of that name (version %d here, %d in the snapshot); the live list was kept — re-author it if the snapshot is right",
		qualifiedName(e.TenantID, e.Name), live.Version, e.Version)
}
