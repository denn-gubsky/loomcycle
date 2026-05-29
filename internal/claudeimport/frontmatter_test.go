package claudeimport

import (
	"strings"
	"testing"
)

func TestSplitFrontmatter_NoFrontmatterBodyOnly(t *testing.T) {
	in := "just a body, no fence at all\nsecond line\n"
	yml, body, err := splitFrontmatter([]byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(yml) != 0 {
		t.Errorf("expected empty yaml, got %q", yml)
	}
	if string(body) != in {
		t.Errorf("body mismatch: got %q want %q", body, in)
	}
}

func TestSplitFrontmatter_FrontmatterWithBody(t *testing.T) {
	in := "---\nname: foo\ntools: \"Read, Write\"\n---\nThis is the body.\nLine two.\n"
	yml, body, err := splitFrontmatter([]byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantYML := "name: foo\ntools: \"Read, Write\""
	if string(yml) != wantYML {
		t.Errorf("yaml mismatch:\n got: %q\nwant: %q", yml, wantYML)
	}
	wantBody := "This is the body.\nLine two.\n"
	if string(body) != wantBody {
		t.Errorf("body mismatch:\n got: %q\nwant: %q", body, wantBody)
	}
}

func TestSplitFrontmatter_FrontmatterNoBody(t *testing.T) {
	in := "---\nname: foo\n---"
	yml, body, err := splitFrontmatter([]byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(yml) != "name: foo" {
		t.Errorf("yaml mismatch: got %q", yml)
	}
	if len(body) != 0 {
		t.Errorf("expected empty body, got %q", body)
	}
}

func TestSplitFrontmatter_MissingClosingFence(t *testing.T) {
	in := "---\nname: foo\n\nThis never closes.\n"
	_, _, err := splitFrontmatter([]byte(in))
	if err == nil {
		t.Fatal("expected error on missing closing ---")
	}
	if !strings.Contains(err.Error(), "no closing") {
		t.Errorf("error should mention missing closing fence, got %q", err.Error())
	}
}

func TestSplitFrontmatter_CRLFNormalised(t *testing.T) {
	in := "---\r\nname: foo\r\n---\r\nbody line\r\n"
	yml, body, err := splitFrontmatter([]byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(yml) != "name: foo" {
		t.Errorf("yaml mismatch: got %q", yml)
	}
	if string(body) != "body line\n" {
		t.Errorf("body mismatch: got %q", body)
	}
}
