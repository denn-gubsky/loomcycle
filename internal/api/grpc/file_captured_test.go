package grpc

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// A file_captured frame carries what the runtime kept, so a gRPC client sees
// the same record the SSE stream and the transcript do.
func TestEventToProto_FileCapturedCarriesTheRecord(t *testing.T) {
	out := eventToProto(providers.Event{Type: providers.EventFileCaptured, File: &providers.FileCapturedInfo{
		Ref: "volume:inbox:documents/a.pdf", MediaType: "application/pdf", Size: 1234,
		SHA256: "ab12", Origin: "mcp:ms365/get", RunID: "run-2",
	}})
	f := out.GetFile()
	if out.GetType() != "file_captured" || f == nil {
		t.Fatalf("event = %+v", out)
	}
	if f.GetRef() != "volume:inbox:documents/a.pdf" || f.GetMediaType() != "application/pdf" || f.GetSize() != 1234 ||
		f.GetSha256() != "ab12" || f.GetOrigin() != "mcp:ms365/get" || f.GetRunId() != "run-2" {
		t.Errorf("file = %+v", f)
	}
	if eventToProto(providers.Event{Type: providers.EventText, Text: "hi"}).GetFile() != nil {
		t.Error("a text frame carries a file")
	}
}
