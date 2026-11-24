package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/loop"
)

// contentSteerConnector records which steer call a RunInput made.
type contentSteerConnector struct {
	mockConnector
	text    string
	content []loop.PromptContentBlock
}

func (c *contentSteerConnector) SteerRun(_ context.Context, _, text, _ string) (bool, error) {
	c.text = text
	return true, nil
}

func (c *contentSteerConnector) SteerRunContent(_ context.Context, _ string, content []loop.PromptContentBlock, _ string) (bool, error) {
	c.content = content
	return true, nil
}

// RunInput with content (RFC HE) hands the blocks, kinds kept, to
// SteerRunContent; with text it is the plain steer, unchanged.
func TestGrpcRunInput_ContentGoesToSteerRunContent(t *testing.T) {
	c := &contentSteerConnector{}
	s := New(Config{Connector: c})
	_, err := s.RunInput(context.Background(), &loomcyclepb.RunInputRequest{RunId: "r1", Content: []*loomcyclepb.PromptContentBlock{
		{Type: "trusted-text", Text: "check this"},
		{Type: "untrusted-block", Kind: "attached_file", Text: `{"ref":"volume:inbox:documents/a.pdf"}`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if c.text != "" || len(c.content) != 2 || c.content[1].Kind != "attached_file" || c.content[1].Type != "untrusted-block" {
		t.Fatalf("text %q content %+v", c.text, c.content)
	}
	c2 := &contentSteerConnector{}
	if _, err := New(Config{Connector: c2}).RunInput(context.Background(), &loomcyclepb.RunInputRequest{RunId: "r1", Text: "go on"}); err != nil || c2.text != "go on" || c2.content != nil {
		t.Fatalf("plain: %v text %q content %+v", err, c2.text, c2.content)
	}
}

// Text beside content is refused before any steer.
func TestGrpcRunInput_TextAndContentTogetherIsInvalid(t *testing.T) {
	c := &contentSteerConnector{}
	_, err := New(Config{Connector: c}).RunInput(context.Background(), &loomcyclepb.RunInputRequest{RunId: "r1", Text: "hi",
		Content: []*loomcyclepb.PromptContentBlock{{Type: "trusted-text", Text: "x"}}})
	if status.Code(err) != codes.InvalidArgument || c.text != "" || c.content != nil {
		t.Fatalf("err %v, steered text %q content %+v", err, c.text, c.content)
	}
}
