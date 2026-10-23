package http

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Gaps the Web UI's Access console found in the management surface. Each
// holds on HTTP and, where the operation has a twin, on the twin too.

// An open-mode server reports its access-control mode like any other: the
// console reads it from /v1/_me to say how rules are applied.
func TestWhoami_OpenModeReportsTheACLMode(t *testing.T) {
	cfg := makeBaseConfig()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "open.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, nil, []tools.Tool{}, concurrency.New(2, 2, time.Second), st)
	srv.SetACLResolver(aclres.New(st, authz.ModeShadow, 0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	code, body := bearerCall(t, ts, "", "GET", "/v1/_me", "")
	if code != 200 || !strings.Contains(body, `"open_mode":true`) || jsonField(t, body, "acl_mode") != "shadow" {
		t.Errorf("open-mode whoami: %d %s, want acl_mode shadow", code, body)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
