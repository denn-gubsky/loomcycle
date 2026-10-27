package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// captureACLMembership takes a snapshot on a store of its own holding one
// group with alice in it and a list bound to that group, in tenant acme.
func captureACLMembership(t *testing.T) []byte {
	t.Helper()
	ctx := context.Background()
	src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	if _, err := src.GroupPut(ctx, store.GroupRow{TenantID: "acme", Name: "support"}); err != nil {
		t.Fatal(err)
	}
	if err := src.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: "acme", GroupName: "support", MemberKind: store.GroupMemberUser, MemberID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.AclDefPut(ctx, store.AclDefRow{TenantID: "acme", Name: "no-shell",
		Rules: json.RawMessage(`[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.AclBindingPut(ctx, store.AclBindingRow{TenantID: "acme", ACLName: "no-shell", SubjectKind: "group", SubjectID: "support"}); err != nil {
		t.Fatal(err)
	}
	_, raw, err := snapshot.Capture(ctx, src, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A restore that brings back access-control rows drops the compiled sets this
// replica cached for their tenant, on every transport, and audits it. The
// resolver trusts a cached acl_version for its whole TTL (an hour here), so
// without the drop a principal resolved before the restore would keep its
// pre-restore set — here, alice would stay outside the restored group and
// escape its deny list.
func TestRestoreSnapshot_DropsTheRestoredTenantsCachedACLSets(t *testing.T) {
	raw := captureACLMembership(t)
	alice := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}}

	for _, tc := range []struct {
		name    string
		restore func(t *testing.T, srv *Server)
	}{
		{"http", func(t *testing.T, srv *Server) {
			body, _ := json.Marshal(map[string]any{"json": json.RawMessage(raw)})
			req := httptest.NewRequest("POST", "/v1/_snapshots/inline/restore", bytes.NewReader(body))
			req.SetPathValue("id", "inline")
			rec := httptest.NewRecorder()
			srv.handleRestoreSnapshot(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("restore status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var resp snapshotRestoreResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if resp.Restored["acl_members"] != 1 || resp.Restored["acl_bindings"] != 1 || resp.Restored["acl_lists"] != 1 {
				t.Errorf("restored map = %v; want one list, member and binding", resp.Restored)
			}
		}},
		{"connector", func(t *testing.T, srv *Server) {
			res, err := srv.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{RawJSON: raw})
			if err != nil {
				t.Fatal(err)
			}
			if res.Restored["acl_members"] != 1 {
				t.Errorf("restored map = %v", res.Restored)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, cleanup := minimalServerWithSnapshotStore(t)
			defer cleanup()
			srv.SetACLResolver(aclres.New(srv.store, authz.ModeCompat, time.Hour))
			sink := &recordingSink{}
			srv.SetTokenAudit(sink)
			ctx := context.Background()

			before, err := srv.aclResolver.Resolve(ctx, alice)
			if err != nil {
				t.Fatal(err)
			}
			if len(before.Groups) != 0 {
				t.Fatalf("groups before the restore = %v", before.Groups)
			}

			tc.restore(t, srv)

			after, err := srv.aclResolver.Resolve(ctx, alice)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after.Groups, []string{"support"}) {
				t.Errorf("groups after the restore = %v, want [support] — the cached pre-restore set is still served", after.Groups)
			}
			found := false
			for _, ev := range sink.events {
				if ev.Action == "acl_restored" && ev.TargetTenant == "acme" {
					found = true
				}
			}
			if !found {
				t.Errorf("audit = %+v, want an acl_restored event for acme", sink.events)
			}
		})
	}
}
