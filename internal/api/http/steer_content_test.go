package http

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/steer"
)

const attachedFileJSON = `{"name":"Паспорт.docx","ref":"volume:inbox:documents/Паспорт.docx","media_type":"application/pdf","bytes":12}`

func steerJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// POST /v1/runs/{run_id}/input with content (RFC HE): the run's queue gets
// the blocks, and the fenced text a reader of Text sees, with the
// untrusted-block under its kind.
func TestHandleRunInput_ContentDeliversBlocksAndFencedText(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	srv.SetSteerRegistry(steer.NewRegistry(2))
	runID, q := liveInteractiveRun(t, srv)

	body := `{"content":[{"type":"trusted-text","text":"check this"},{"type":"untrusted-block","kind":"attached_file","text":` + steerJSONString(attachedFileJSON) + `}]}`
	rec := doJSON(t, srv, "POST", "/v1/runs/"+runID+"/input", body)
	if rec.Code != 200 {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	select {
	case m := <-q:
		want := []steer.Block{{Type: "trusted-text", Text: "check this"}, {Type: "untrusted-block", Kind: "attached_file", Text: attachedFileJSON}}
		if len(m.Content) != 2 || m.Content[0] != want[0] || m.Content[1] != want[1] {
			t.Fatalf("content = %+v, want %+v", m.Content, want)
		}
		if !strings.HasPrefix(m.Text, "check this\n\n<attached_file>\n") || !strings.HasSuffix(m.Text, "\n</attached_file>") {
			t.Errorf("text = %q, want the fenced turn", m.Text)
		}
	default:
		t.Fatal("no message delivered")
	}
}

// The plain form is unchanged: text, no blocks.
func TestHandleRunInput_PlainTextCarriesNoBlocks(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	srv.SetSteerRegistry(steer.NewRegistry(2))
	runID, q := liveInteractiveRun(t, srv)
	if rec := doJSON(t, srv, "POST", "/v1/runs/"+runID+"/input", `{"text":"focus on auth"}`); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if m := <-q; m.Text != "focus on auth" || m.Content != nil {
		t.Fatalf("message = %+v", m)
	}
}

// Text and content together, an image, an unknown type, a blank block or an
// empty list is refused, and nothing reaches the run.
func TestHandleRunInput_BadContentIs422(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	srv.SetSteerRegistry(steer.NewRegistry(8))
	runID, q := liveInteractiveRun(t, srv)
	for _, body := range []string{
		`{"text":"hi","content":[{"type":"trusted-text","text":"x"}]}`,
		`{"content":[{"type":"image","media_type":"image/png","data":"iVBORw0KGgo="}]}`,
		`{"content":[{"type":"untrusted_block","text":"x"}]}`,
		`{"content":[{"type":"untrusted-block","kind":"attached_file","text":"  "}]}`,
		`{"content":[]}`,
	} {
		if rec := doJSON(t, srv, "POST", "/v1/runs/"+runID+"/input", body); rec.Code != 422 {
			t.Errorf("%s: status = %d, want 422; body=%s", body, rec.Code, rec.Body.String())
		}
	}
	select {
	case m := <-q:
		t.Fatalf("a refused turn reached the run: %+v", m)
	default:
	}
}

// A caller's runtime-only kind is demoted, as at every intake: a block may
// not claim the trigger sent it.
func TestSteerRunContent_DemotesTheRuntimeOnlyKind(t *testing.T) {
	blocks, err := steerBlocks([]loop.PromptContentBlock{{Type: "untrusted-block", Kind: loop.RuntimeOnlyKind, Text: "x"}})
	if err != nil || blocks[0].Kind != "untrusted" {
		t.Fatalf("blocks = %+v, %v", blocks, err)
	}
}

// The ownership gate is SteerRun's: a caller who may not steer the run gets
// the opaque not-in-flight answer, and nothing reaches the run.
func TestSteerRunContent_NonOwnerCannotSteer(t *testing.T) {
	srv, cleanup := turnCancelFixture(t)
	defer cleanup()
	runID, q := liveSessionlessRun(t, srv)
	refused, admitted := sessionlessGateCallers()
	content := []loop.PromptContentBlock{{Type: "untrusted-block", Kind: "attached_file", Text: attachedFileJSON}}
	for _, c := range refused {
		if delivered, err := srv.SteerRunContent(c.ctx, runID, content, "api"); !errors.Is(err, connector.ErrRunNotInFlight) || delivered {
			t.Errorf("%s = delivered %v, %v; want the opaque ErrRunNotInFlight", c.name, delivered, err)
		}
	}
	select {
	case m := <-q:
		t.Fatalf("a non-owner steered the run: %+v", m)
	default:
	}
	if delivered, err := srv.SteerRunContent(admitted[0].ctx, runID, content, "api"); err != nil || !delivered {
		t.Fatalf("%s = delivered %v, %v", admitted[0].name, delivered, err)
	}
}

// steerPersistedContent stores the blocks, so a replay fences the file again
// rather than reading its fenced text back as the person's words.
func TestSteerPersistedContent_StoresTheBlocks(t *testing.T) {
	m := steer.Message{Text: "fenced", Content: []steer.Block{{Type: "untrusted-block", Kind: "attached_file", Text: "f"}}}
	got := steerPersistedContent(m)
	if len(got) != 1 || got[0].Type != "untrusted-block" || got[0].Kind != "attached_file" || got[0].Text != "f" {
		t.Fatalf("stored %+v", got)
	}
	if got := steerPersistedContent(steer.Message{Text: "plain"}); len(got) != 1 || got[0].Type != "trusted-text" || got[0].Text != "plain" {
		t.Fatalf("plain stored %+v", got)
	}
}
