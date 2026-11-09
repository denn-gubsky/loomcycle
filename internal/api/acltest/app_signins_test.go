package acltest

import (
	"context"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The app sign-in operations give one answer on HTTP and gRPC: an operator
// denied a user by its access control reaches neither that user's list nor
// its sign-ins, a member is refused the operator twins, and the caller's own
// twins answer for itself only.
func TestAppSignIns_OneAnswerOnEveryTransport(t *testing.T) {
	var st store.Store
	e := newEnvWith(t, authz.ModeCompat, func(_ *config.Config, s store.Store) ([]tools.Tool, func(*lchttp.Server)) {
		st = s
		return nil, func(*lchttp.Server) {}
	})
	ctx := context.Background()
	now := time.Now().UTC()
	for _, sub := range []string{"victim", "other", "alice"} {
		if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: sub, AccessMode: "tenant", Status: "active"}); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"1", "2"} {
			if err := st.OAuthFamilyCreate(ctx, store.OAuthFamilyRow{FamilyID: "fam-" + sub + n, ClientID: "board", TenantID: "acme",
				Subject: sub, Gate: "idp", CreatedAt: now, LastUsedAt: now, IdleExpiresAt: now.Add(time.Hour),
				AbsoluteExpiresAt: now.Add(24 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	rules := `[{"effect":"deny","actions":["read","manage"],"resources":["principal:user:victim"]}]`
	for _, c := range []struct{ path, body string }{
		{"/v1/_acldef", `{"op":"create","name":"hands-off","rules":` + rules + `}`},
		{"/v1/_aclbinding", `{"acl":"hands-off","subject_kind":"user","subject_id":"delegate"}`},
	} {
		if resp, raw := e.do(http.MethodPost, c.path, operator, c.body, nil); resp.StatusCode != http.StatusCreated {
			t.Fatalf("seed %s: %d %s", c.path, resp.StatusCode, raw)
		}
	}
	call := func(bearer string) context.Context {
		return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+bearer)
	}
	codeOfErr := func(err error) codes.Code { return status.Code(err) }

	// The denied operator: refused on both transports, for listing and ending.
	if resp, raw := e.do(http.MethodGet, "/v1/_users/victim/app-sign-ins", delegate, "", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("HTTP list of the denied user = %d %s", resp.StatusCode, raw)
	}
	if _, err := e.grpc.ListUserAppSignIns(call(delegate), &loomcyclepb.ListUserAppSignInsRequest{Subject: "victim"}); codeOfErr(err) != codes.PermissionDenied {
		t.Errorf("gRPC list of the denied user = %v", err)
	}
	if _, err := e.grpc.EndUserAppSignIn(call(delegate), &loomcyclepb.EndUserAppSignInRequest{Subject: "victim", FamilyId: "fam-victim1"}); codeOfErr(err) != codes.PermissionDenied {
		t.Errorf("gRPC end of the denied user's sign-in = %v", err)
	}
	if f, _ := st.OAuthFamilyGet(ctx, "fam-victim1"); !f.RevokedAt.IsZero() {
		t.Fatal("a refused end revoked the sign-in")
	}
	// The same operator reaches a user it is not denied, on both.
	if resp, raw := e.do(http.MethodGet, "/v1/_users/other/app-sign-ins", delegate, "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("HTTP list of an undenied user = %d %s", resp.StatusCode, raw)
	}
	got, err := e.grpc.ListUserAppSignIns(call(delegate), &loomcyclepb.ListUserAppSignInsRequest{Subject: "other"})
	if err != nil || len(got.GetAppSignIns()) != 2 || got.GetAppSignIns()[0].GetExpiresAt() == "" {
		t.Fatalf("gRPC list of an undenied user = %v, %v", got, err)
	}
	if _, err := e.grpc.EndUserAppSignIn(call(delegate), &loomcyclepb.EndUserAppSignInRequest{Subject: "other", FamilyId: "fam-other1"}); err != nil {
		t.Fatalf("gRPC end of an undenied user's sign-in: %v", err)
	}
	if f, _ := st.OAuthFamilyGet(ctx, "fam-other1"); f.RevokedAt.IsZero() {
		t.Fatal("the gRPC end left the sign-in live")
	}
	// Another user's family through this user's name is not found.
	if _, err := e.grpc.EndUserAppSignIn(call(operator), &loomcyclepb.EndUserAppSignInRequest{Subject: "other", FamilyId: "fam-victim2"}); codeOfErr(err) != codes.NotFound {
		t.Errorf("gRPC end of another user's family = %v", err)
	}

	// A member is refused the operator twins on both transports.
	if resp, _ := e.do(http.MethodGet, "/v1/_users/other/app-sign-ins", member, "", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("HTTP: a member reached the operator route: %d", resp.StatusCode)
	}
	if _, err := e.grpc.ListUserAppSignIns(call(member), &loomcyclepb.ListUserAppSignInsRequest{Subject: "other"}); codeOfErr(err) != codes.PermissionDenied {
		t.Errorf("gRPC: a member reached the operator twin: %v", err)
	}
	// Its own twins answer for itself, and end only its own.
	mine, err := e.grpc.ListMyAppSignIns(call(member), &loomcyclepb.ListMyAppSignInsRequest{})
	if err != nil || mine.GetSubject() != "alice" || len(mine.GetAppSignIns()) != 2 {
		t.Fatalf("gRPC own list = %v, %v", mine, err)
	}
	if _, err := e.grpc.EndMyAppSignIn(call(member), &loomcyclepb.EndMyAppSignInRequest{FamilyId: "fam-other2"}); codeOfErr(err) != codes.NotFound {
		t.Errorf("gRPC: a member ended another person's sign-in: %v", err)
	}
	if _, err := e.grpc.EndMyAppSignIn(call(member), &loomcyclepb.EndMyAppSignInRequest{FamilyId: "fam-alice1"}); err != nil {
		t.Fatalf("gRPC own end: %v", err)
	}
	if f, _ := st.OAuthFamilyGet(ctx, "fam-alice1"); f.RevokedAt.IsZero() || f.RevokeReason != "signed_out_by_user" {
		t.Fatalf("own end left %+v", f)
	}
	if f, _ := st.OAuthFamilyGet(ctx, "fam-other2"); !f.RevokedAt.IsZero() {
		t.Fatal("a refused own end revoked another person's sign-in")
	}
}
