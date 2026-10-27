package providers

import "context"

// AudioTranscriber is a driver that serves a speech-to-text endpoint in the
// OpenAI shape (POST {base_url}/audio/transcriptions, multipart). One request
// per call: the File tool cuts a recording into pieces the endpoint accepts and
// calls this once for each.
//
// The call resolves its key exactly as a chat call of the same driver does: a
// tenant's or user's own credential for the driver's key name overrides the
// operator's, and a run restricted from the operator's key is refused with
// ErrOperatorKeyForbidden rather than sent on it.
//
// The registry hands drivers out wrapped, so reach this through As, never a
// type assertion.
type AudioTranscriber interface {
	TranscribeAudio(ctx context.Context, req AudioRequest) (AudioResponse, error)
}

// AudioRequest is one transcription request. Every value is the runtime's: the
// model and fields come from operator configuration or have been sanitised
// (the language against a closed list, the vocabulary folded to one line).
type AudioRequest struct {
	// Model is the concrete model id the endpoint serves.
	Model string
	// File is the audio, sent as the multipart "file" part under Filename;
	// the endpoint reads its format from the file name's extension.
	File     []byte
	Filename string
	// Fields are the other form fields (language, prompt, response_format,
	// chunking_strategy …). An empty value is not sent.
	Fields map[string]string
}

// AudioResponse is the endpoint's answer: its JSON body as it came, bounded,
// and whose key paid.
type AudioResponse struct {
	Body []byte
	// CredentialSource is "operator", or the scope of the run's own
	// credential ("tenant", "user", "agent"); CredentialScopeID names its
	// owner.
	CredentialSource  string
	CredentialScopeID string
}

// MaxAudioResponseBytes bounds the body a transcription answer may have: the
// text of hours of speech with its segments is a few megabytes.
const MaxAudioResponseBytes = 16 << 20
