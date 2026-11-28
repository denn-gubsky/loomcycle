package builtin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// RFC HH P2: the Workspace tool asks for any well-formed toolchain, and
// lists the ones on offer.

// devFixture is a fixture with the generic worker w1 (python, node) and a
// second one of the same tenant, zdev, carrying python, go and cpp but not
// node. Placement is least loaded first, ties to the lower id: w1.
func devFixture(t *testing.T) (*wsFixture, string) {
	t.Helper()
	f := newWSFixture(t, lifecycle.Limits{MaxPerRun: 8})
	return f, f.h.AddWorkerWithToolchains("zdev", 10, []string{"python", "go", "cpp"})
}

func TestWorkspaceOpen_ToolchainsPlaceOnTheWorkerCarryingAll(t *testing.T) {
	f, dev := devFixture(t)
	ctx := f.runCtx(t, "alice", "", wsFull)
	// Both workers are empty, and a tie goes to w1: only the toolchains
	// can send this to zdev.
	f.mustOK(t, ctx, `{"op":"open","name":"cxx","toolchains":["go","cpp","go"]}`)
	if got := f.row(t, ctx, "cxx").WorkerID; got != dev {
		t.Errorf("toolchains [go, cpp]: on %s, want %s", got, dev)
	}
	// Load w1 past zdev: python alone goes where it is least loaded.
	f.mustOK(t, ctx, `{"op":"open","name":"py","toolchain":"python"}`)
	f.mustOK(t, ctx, `{"op":"open","name":"py2","toolchain":"python"}`)
	if got := f.row(t, ctx, "py").WorkerID; got != f.h.WorkerID {
		t.Fatalf("toolchain python: on %s, want %s (the less loaded)", got, f.h.WorkerID)
	}
	// The general workspace is unchanged: python and node, so never zdev,
	// though zdev is now the less loaded.
	f.mustOK(t, ctx, `{"op":"open","name":"plain"}`)
	if got := f.row(t, ctx, "plain").WorkerID; got != f.h.WorkerID {
		t.Errorf("general workspace: on %s, want %s", got, f.h.WorkerID)
	}
}

func TestWorkspaceOpen_RefusesMalformedOrConflictingToolchains(t *testing.T) {
	f, _ := devFixture(t)
	// roots too, so a root open reaches the toolchain check.
	rooted := wsFull
	rooted.Roots = true
	ctx := f.runCtx(t, "alice", "", rooted)
	many := make([]string, 33)
	for i := range many {
		many[i] = "t" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	manyJSON, _ := json.Marshal(many)
	for _, in := range []string{
		`{"op":"open","name":"w","toolchain":"Bad Name"}`,
		`{"op":"open","name":"w","toolchains":["go","c++ -x"]}`,
		`{"op":"open","name":"w","toolchains":[]}`,
		`{"op":"open","name":"w","toolchain":"go","toolchains":["cpp"]}`,
		`{"op":"open","name":"w","toolchains":` + string(manyJSON) + `}`,
		`{"op":"open","name":"w","template":"t","toolchains":["go"]}`,
		`{"op":"open","name":"w","root":"work","toolchains":["go"]}`,
	} {
		if c := refusalClass(f.call(t, ctx, in)); c != "invalid_argument" {
			t.Errorf("%s = %q, want invalid_argument", in, c)
		}
	}
	res := f.call(t, ctx, `{"op":"open","name":"w","toolchains":["cpp","rust"]}`)
	if refusalClass(res) != "no_worker" || !strings.Contains(res.Text, "rust") || !strings.Contains(res.Text, "on offer: cpp, go, node, python") {
		t.Errorf("a toolchain no worker carries = %s, want no_worker naming rust and the offer", res.Text)
	}
}

type offeredToolchain struct {
	Name    string   `json:"name"`
	Engines []string `json:"engines"`
}

func TestWorkspaceToolchains_ListsTheCallersOfferByGrant(t *testing.T) {
	f, _ := devFixture(t)
	ctx := f.runCtx(t, "alice", "", wsFull)
	var got struct {
		Toolchains []offeredToolchain `json:"toolchains"`
	}
	if err := json.Unmarshal([]byte(f.mustOK(t, ctx, `{"op":"toolchains"}`).Text), &got); err != nil {
		t.Fatal(err)
	}
	want := []offeredToolchain{{"cpp", []string{"host"}}, {"go", []string{"host"}}, {"node", []string{"host"}}, {"python", []string{"host"}}}
	if !reflect.DeepEqual(got.Toolchains, want) {
		t.Errorf("toolchains = %+v, want %+v", got.Toolchains, want)
	}
	if strings.Contains(f.mustOK(t, ctx, `{"op":"toolchains"}`).Text, "zdev") {
		t.Error("the offer names a worker")
	}

	// A grant requiring the container engine: the host workers offer nothing.
	boxed := wsFull
	boxed.Engine = config.WorkspaceEngineContainer
	res := f.mustOK(t, f.runCtx(t, "alice", "", boxed), `{"op":"toolchains"}`)
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil || len(got.Toolchains) != 0 {
		t.Errorf("container required: %s, want none on offer", res.Text)
	}

	// It is open's: a grant that may not open is refused.
	readOnly := wsFull
	readOnly.Open = false
	if c := refusalClass(f.call(t, f.runCtx(t, "alice", "", readOnly), `{"op":"toolchains"}`)); c != "not_granted" {
		t.Errorf("without open: %q, want not_granted", c)
	}
}
