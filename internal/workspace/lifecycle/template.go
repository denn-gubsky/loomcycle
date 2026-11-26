package lifecycle

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
)

// Workspace templates (RFC GW §5.5, P6). A template is a checkpoint packed
// on its worker into one archive the core keeps under its data directory,
// with its tenant, scope, toolchains and digest on a row. The core never
// opens the archive: it copies the bytes in from the worker that packed it
// and out to a worker that opens it, which keeps it by digest. A template
// is content of its tenant and never crosses to another (D-P6-24).

var (
	// ErrTemplatesOff: the runtime keeps no templates (no folder set).
	ErrTemplatesOff = errors.New("this runtime keeps no workspace templates")
	// ErrTemplateNotFound: no template of that name the caller may use.
	ErrTemplateNotFound = errors.New("no template of that name")
	// ErrTemplateLimit: the tenant holds as many templates as it may.
	ErrTemplateLimit = errors.New("template limit reached")
	// ErrTemplateTooLarge: the archive passes workspace.template.max_mb.
	ErrTemplateTooLarge = errors.New("the template is larger than allowed")
	// ErrTemplateExists: a template of that name is held in that scope.
	ErrTemplateExists = errors.New("a template of that name exists")
	// ErrTemplateScope: a scope the caller may not save in or name.
	ErrTemplateScope = errors.New("invalid template scope")
)

// DefaultTemplateMaxBytes and DefaultMaxTemplates are config
// workspace.template's defaults, for a Limits that names none.
const (
	DefaultTemplateMaxBytes    = 1 << 30
	DefaultMaxTemplates        = 20
	DefaultMaxTemplatesPerUser = 5
)

const templateIDPrefix = "tpl_"

func newTemplateID() (string, error) {
	var b [12]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return templateIDPrefix + hex.EncodeToString(b[:]), nil
}

// validTemplateID reports whether id is one newTemplateID makes, so it is
// never a path.
func validTemplateID(id string) bool {
	rest, ok := strings.CutPrefix(id, templateIDPrefix)
	if !ok || len(rest) != 24 {
		return false
	}
	_, err := hex.DecodeString(rest)
	return err == nil
}

// SetTemplateDir sets the folder template archives are kept in. Boot
// wiring only; an empty one keeps none.
func (m *Manager) SetTemplateDir(dir string) { m.templateDir = dir }

func (m *Manager) templatePath(id string) (string, error) {
	return TemplateArchivePath(m.templateDir, id)
}

// TemplateArchivePath is where the archive of template id is kept in dir:
// what SaveTemplate writes and a subject's erasure removes. id must be one
// newTemplateID makes, so the path never leaves dir.
func TemplateArchivePath(dir, id string) (string, error) {
	if dir == "" {
		return "", ErrTemplatesOff
	}
	if !validTemplateID(id) {
		return "", fmt.Errorf("template %q: not a template id", id)
	}
	return filepath.Join(dir, id+".tar.gz"), nil
}

func (m *Manager) templateMaxBytes() int64 {
	if m.limits.TemplateMaxBytes > 0 {
		return m.limits.TemplateMaxBytes
	}
	return DefaultTemplateMaxBytes
}

func (m *Manager) maxTemplatesPerUser() int {
	if m.limits.MaxTemplatesPerUser > 0 {
		return m.limits.MaxTemplatesPerUser
	}
	return DefaultMaxTemplatesPerUser
}

// userTemplateCount counts a user's user templates: a run and a session of
// the MCP server of one subject share them (D-P6-45), and so share the
// per-user bound that keeps one member from filling the tenant's.
func (m *Manager) userTemplateCount(ctx context.Context, tenantID, user string) (int, error) {
	rows, err := m.store.WorkspaceTemplateList(ctx, tenantID, store.WorkspaceTemplateUser, user)
	return len(rows), err
}

func (m *Manager) userTemplateLimit(n int) error {
	return fmt.Errorf("%w: you hold %d user templates (workspace.template.max_per_user); delete one first", ErrTemplateLimit, n)
}

func (m *Manager) maxTemplates() int {
	if m.limits.MaxTemplates > 0 {
		return m.limits.MaxTemplates
	}
	return DefaultMaxTemplates
}

// templateScopeID is the scope id a template of scope is kept under for o:
// its user for a user template, none for a tenant one.
func templateScopeID(o Owner, scope string) (string, error) {
	switch scope {
	case store.WorkspaceTemplateTenant:
		return "", nil
	case store.WorkspaceTemplateUser:
		if o.UserID == "" {
			return "", fmt.Errorf("%w: a user template belongs to a user, and this run has none", ErrTemplateScope)
		}
		return o.UserID, nil
	}
	return "", fmt.Errorf("%w: %q (want user or tenant)", ErrTemplateScope, scope)
}

// SaveTemplate packs checkpoint of the caller's own workspace src into a
// template named as, in scope. The caller's grant (workspace.templates,
// and the tenant value for a tenant template) is the tool's to check; this
// checks the source is the caller's, the scope, the name and the bounds.
// A name already held in that scope is ErrTemplateExists: a template is never
// overwritten.
func (m *Manager) SaveTemplate(ctx context.Context, o Owner, src store.WorkspaceRow, checkpoint, as, scope string) (store.WorkspaceTemplateRow, error) {
	if o.RunID == "" || src.OwnerRunID != o.RunID || src.TenantID != o.TenantID || src.OwnerKind != o.kind() {
		return store.WorkspaceTemplateRow{}, ErrNotFound
	}
	if !ValidName(as) {
		return store.WorkspaceTemplateRow{}, fmt.Errorf("%w: as: 1 to %d letters, digits, '.', '-' or '_', starting with a letter or digit", ErrInvalidName, MaxNameLen)
	}
	scopeID, err := templateScopeID(o, scope)
	if err != nil {
		return store.WorkspaceTemplateRow{}, err
	}
	if src.State != store.WorkspaceReady {
		return store.WorkspaceTemplateRow{}, fmt.Errorf("%w: %q is %s", ErrNotReady, src.Name, src.State)
	}
	if _, err := m.store.WorkspaceTemplateGet(ctx, o.TenantID, scope, scopeID, as); err == nil {
		return store.WorkspaceTemplateRow{}, fmt.Errorf("%w: a %s template %q", ErrTemplateExists, scope, as)
	} else if !isNotFound(err) {
		return store.WorkspaceTemplateRow{}, err
	}
	if scope == store.WorkspaceTemplateUser {
		if n, err := m.userTemplateCount(ctx, o.TenantID, scopeID); err != nil {
			return store.WorkspaceTemplateRow{}, err
		} else if n >= m.maxTemplatesPerUser() {
			return store.WorkspaceTemplateRow{}, m.userTemplateLimit(n)
		}
	}
	if n, err := m.store.WorkspaceTemplateCount(ctx, o.TenantID); err != nil {
		return store.WorkspaceTemplateRow{}, err
	} else if n >= m.maxTemplates() {
		return store.WorkspaceTemplateRow{}, fmt.Errorf("%w: the tenant holds %d templates (workspace.template.max_per_tenant); delete one first", ErrTemplateLimit, n)
	}
	id, err := newTemplateID()
	if err != nil {
		return store.WorkspaceTemplateRow{}, err
	}
	path, err := m.templatePath(id)
	if err != nil {
		return store.WorkspaceTemplateRow{}, err
	}
	digest, size, err := m.packTemplate(ctx, src, checkpoint, path)
	if err != nil {
		return store.WorkspaceTemplateRow{}, err
	}
	row := store.WorkspaceTemplateRow{ID: id, TenantID: o.TenantID, Scope: scope, ScopeID: scopeID, Name: as,
		Toolchains: m.workerToolchains(src.WorkerID), Engine: src.Engine, Digest: digest, Bytes: size,
		AuthorUser: o.UserID, AuthorRun: o.RunID}
	out, err := m.store.WorkspaceTemplateCreate(ctx, row)
	if err != nil {
		_ = os.Remove(path)
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			return store.WorkspaceTemplateRow{}, fmt.Errorf("%w: a %s template %q", ErrTemplateExists, scope, as)
		}
		return store.WorkspaceTemplateRow{}, err
	}
	// Counted again with the row written: two saves racing each see the
	// other's row, so neither passes the bound both together would break.
	if scope == store.WorkspaceTemplateUser {
		if n, err := m.userTemplateCount(ctx, o.TenantID, scopeID); err != nil || n > m.maxTemplatesPerUser() {
			m.removeTemplate(out)
			if err != nil {
				return store.WorkspaceTemplateRow{}, err
			}
			return store.WorkspaceTemplateRow{}, m.userTemplateLimit(n - 1)
		}
	}
	if n, err := m.store.WorkspaceTemplateCount(ctx, o.TenantID); err != nil || n > m.maxTemplates() {
		m.removeTemplate(out)
		if err != nil {
			return store.WorkspaceTemplateRow{}, err
		}
		return store.WorkspaceTemplateRow{}, fmt.Errorf("%w: the tenant holds %d templates (workspace.template.max_per_tenant); delete one first", ErrTemplateLimit, n-1)
	}
	m.recordTemplate(o, out, "workspace_template_save", src.ID)
	return out, nil
}

// packTemplate streams the checkpoint's archive from the source's worker
// into path, and answers its SHA-256 and size. Past the bound nothing is
// kept.
func (m *Manager) packTemplate(ctx context.Context, src store.WorkspaceRow, checkpoint, path string) (string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", 0, err
	}
	part := path + ".part"
	// #nosec G304 -- path is the template folder joined with a minted template id
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	kept := false
	defer func() {
		if !kept {
			_ = f.Close()
			_ = os.Remove(part)
		}
	}()
	args, err := json.Marshal(workspace.TemplatePackArgs{Checkpoint: checkpoint})
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	limit := m.templateMaxBytes()
	_, err = m.disp.Stream(ctx, src.WorkerID, dispatch.Request{TenantID: src.TenantID, WorkspaceID: src.ID,
		Op: workspace.OpTemplatePack, Args: args, MaxOutput: limit}, nil, cw)
	if dispatch.ClassOf(err) == workspace.ClassOutputTooLarge {
		return "", 0, fmt.Errorf("%w: the template passes %d bytes (workspace.template.max_mb); nothing was kept", ErrTemplateTooLarge, limit)
	}
	if err != nil {
		return "", 0, err
	}
	if cw.n == 0 {
		return "", 0, &dispatch.OpError{Class: workspace.ClassProtocol, Detail: "the worker sent an empty template"}
	}
	if err := f.Sync(); err != nil {
		return "", 0, err
	}
	if err := f.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(part, path); err != nil {
		return "", 0, err
	}
	kept = true
	return hex.EncodeToString(h.Sum(nil)), cw.n, nil
}

func isNotFound(err error) bool {
	var nf *store.ErrNotFound
	return errors.As(err, &nf)
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// workerToolchains is what the worker that holds a workspace carries: what
// a template saved from it needs (D-P6-31).
func (m *Manager) workerToolchains(workerID string) []string {
	for _, w := range m.disp.Workers() {
		if w.ID == workerID {
			return append([]string{}, w.Toolchains...)
		}
	}
	return []string{}
}

// FindTemplate is the template name the caller may open (D-P6-24, D-P6-25):
// its user's own first, then its tenant's; never another tenant's, never
// another user's. A principal (a session of the MCP server) is its token's
// tenant and subject, the subject in the user's place (ruled 2026-11-25).
func (m *Manager) FindTemplate(ctx context.Context, o Owner, name string) (store.WorkspaceTemplateRow, error) {
	if o.RunID == "" || !ValidName(name) {
		return store.WorkspaceTemplateRow{}, ErrTemplateNotFound
	}
	if o.UserID != "" {
		r, err := m.store.WorkspaceTemplateGet(ctx, o.TenantID, store.WorkspaceTemplateUser, o.UserID, name)
		if err == nil {
			return r, nil
		}
		if !isNotFound(err) {
			return store.WorkspaceTemplateRow{}, err
		}
	}
	r, err := m.store.WorkspaceTemplateGet(ctx, o.TenantID, store.WorkspaceTemplateTenant, "", name)
	if isNotFound(err) {
		return store.WorkspaceTemplateRow{}, fmt.Errorf("%w: %q", ErrTemplateNotFound, name)
	}
	return r, err
}

// GetTemplate is the template of that scope and name the caller may
// delete: a user template only its own user's.
func (m *Manager) GetTemplate(ctx context.Context, o Owner, scope, name string) (store.WorkspaceTemplateRow, error) {
	if o.RunID == "" {
		return store.WorkspaceTemplateRow{}, ErrTemplateNotFound
	}
	scopeID, err := templateScopeID(o, scope)
	if err != nil {
		return store.WorkspaceTemplateRow{}, err
	}
	r, err := m.store.WorkspaceTemplateGet(ctx, o.TenantID, scope, scopeID, name)
	if isNotFound(err) {
		return store.WorkspaceTemplateRow{}, fmt.Errorf("%w: a %s template %q", ErrTemplateNotFound, scope, name)
	}
	return r, err
}

// Templates lists the templates the caller may open: its user's, then its
// tenant's.
func (m *Manager) Templates(ctx context.Context, o Owner) ([]store.WorkspaceTemplateRow, error) {
	out := []store.WorkspaceTemplateRow{}
	if o.RunID == "" {
		return out, nil
	}
	if o.UserID != "" {
		mine, err := m.store.WorkspaceTemplateList(ctx, o.TenantID, store.WorkspaceTemplateUser, o.UserID)
		if err != nil {
			return nil, err
		}
		out = append(out, mine...)
	}
	theirs, err := m.store.WorkspaceTemplateList(ctx, o.TenantID, store.WorkspaceTemplateTenant, "")
	if err != nil {
		return nil, err
	}
	return append(out, theirs...), nil
}

// TenantTemplates lists every template of the caller's tenant, every
// user's included: a tenant operator's listing (D-P6-48). The caller's
// authority is the tool's to check.
func (m *Manager) TenantTemplates(ctx context.Context, o Owner) ([]store.WorkspaceTemplateRow, error) {
	if o.RunID == "" || o.TenantID == "" {
		return []store.WorkspaceTemplateRow{}, nil
	}
	out, err := m.store.WorkspaceTemplateListTenant(ctx, o.TenantID)
	if out == nil {
		out = []store.WorkspaceTemplateRow{}
	}
	return out, err
}

// UserTemplateOf is user owner's template name in the caller's tenant: what
// a tenant operator deletes for another user (D-P6-48). Never another
// tenant's: the tenant is the caller's.
func (m *Manager) UserTemplateOf(ctx context.Context, o Owner, owner, name string) (store.WorkspaceTemplateRow, error) {
	if o.RunID == "" || owner == "" {
		return store.WorkspaceTemplateRow{}, ErrTemplateNotFound
	}
	r, err := m.store.WorkspaceTemplateGet(ctx, o.TenantID, store.WorkspaceTemplateUser, owner, name)
	if isNotFound(err) {
		return store.WorkspaceTemplateRow{}, fmt.Errorf("%w: %q of %q", ErrTemplateNotFound, name, owner)
	}
	return r, err
}

// DeleteTemplate removes a template the caller found with GetTemplate: its
// row, then its archive. A worker that cached it keeps the tree until it is
// evicted; nothing can name it any more (D-P6-30).
func (m *Manager) DeleteTemplate(ctx context.Context, o Owner, r store.WorkspaceTemplateRow) error {
	if r.TenantID != o.TenantID {
		return ErrTemplateNotFound
	}
	found, err := m.store.WorkspaceTemplateDelete(ctx, r.TenantID, r.ID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %q", ErrTemplateNotFound, r.Name)
	}
	m.removeArchive(r.ID)
	m.recordTemplate(o, r, "workspace_template_delete", "")
	return nil
}

// removeTemplate undoes a save: the row and the archive.
func (m *Manager) removeTemplate(r store.WorkspaceTemplateRow) {
	if _, err := m.store.WorkspaceTemplateDelete(context.Background(), r.TenantID, r.ID); err != nil {
		m.log.Warn("cannot remove a template's row", "template_id", r.ID, "err", err)
	}
	m.removeArchive(r.ID)
}

func (m *Manager) removeArchive(id string) {
	path, err := m.templatePath(id)
	if err != nil {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.log.Warn("cannot remove a template's archive", "template_id", id, "err", err)
	}
}

// openFromTemplate makes workspace id on workerID as a copy of the template:
// the worker copies it from its cache, and a worker that does not hold it is
// sent the archive once and asked again.
func (m *Manager) openFromTemplate(ctx context.Context, o Owner, t store.WorkspaceTemplateRow, workerID, id string) error {
	args, err := json.Marshal(workspace.TemplateOpenArgs{Digest: t.Digest, DiskQuotaBytes: m.limits.DiskQuotaBytes})
	if err != nil {
		return err
	}
	req := dispatch.Request{TenantID: o.TenantID, WorkspaceID: id, Op: workspace.OpTemplateOpen, Args: args, MaxOutput: maxCheckpointAnswer}
	_, err = m.disp.Do(ctx, workerID, req)
	if dispatch.ClassOf(err) != workspace.ClassTemplateMissing {
		return err
	}
	if err := m.sendTemplate(ctx, o, t, workerID, id); err != nil {
		return err
	}
	_, err = m.disp.Do(ctx, workerID, req)
	return err
}

// sendTemplate streams the template's archive to the worker's cache.
func (m *Manager) sendTemplate(ctx context.Context, o Owner, t store.WorkspaceTemplateRow, workerID, id string) error {
	path, err := m.templatePath(t.ID)
	if err != nil {
		return err
	}
	// #nosec G304 -- path is the template folder joined with a checked template id
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("the template's archive: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	unpacked := m.limits.DiskQuotaBytes
	if unpacked <= 0 {
		unpacked = workspace.DefaultDiskQuotaBytes
	}
	args, err := json.Marshal(workspace.TemplatePutArgs{Digest: t.Digest, Size: st.Size(), MaxBytes: unpacked})
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	_, err = m.disp.Stream(ctx, workerID, dispatch.Request{TenantID: o.TenantID, WorkspaceID: id, Op: workspace.OpTemplatePut,
		Args: args, MaxOutput: maxAnswer, InputSize: st.Size()}, f, &buf)
	return err
}

// templateOwner is the user a template belongs to, for its audit line: a
// user template's user, none for a tenant one. With an operator's delete
// the line so names both the operator (the actor) and the owner.
func templateOwner(t store.WorkspaceTemplateRow) string {
	if t.Scope == store.WorkspaceTemplateUser {
		return t.ScopeID
	}
	return ""
}

// recordTemplate writes the audit line of a template saved or deleted
// (D-P6-35): the tenant, scope, name, digest and size, the run, and for a
// save the workspace it was made from. Never a file's content.
func (m *Manager) recordTemplate(o Owner, t store.WorkspaceTemplateRow, action, fromWorkspace string) {
	if m.audit == nil {
		return
	}
	_ = m.audit.Record(audit.Event{
		Action: action, ActorTenant: o.TenantID, ActorSubject: o.UserID,
		TargetTenant: t.TenantID, TargetName: "workspace_template:" + t.Name, TargetSubject: templateOwner(t),
		WorkspaceID: fromWorkspace, WorkspaceOwnerRunID: o.RunID, WorkspaceOwnerAgentID: o.AgentID,
		WorkspaceTemplateID: t.ID, WorkspaceTemplateScope: t.Scope, WorkspaceTemplateDigest: t.Digest, WorkspaceTemplateBytes: t.Bytes,
	}) // best effort, as for shares
}
