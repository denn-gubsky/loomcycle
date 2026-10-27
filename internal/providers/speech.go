package providers

import "context"

// SpeechSynthesizer is a driver that serves a text-to-speech endpoint in the
// OpenAI shape (POST {base_url}/audio/speech, JSON in, audio out). One request
// per call: the runtime cuts long text into pieces the endpoint accepts, calls
// this once for each, and joins the audio itself.
//
// The call resolves its key exactly as a chat call of the same driver does: a
// tenant's or user's own credential for the driver's key name overrides the
// operator's, and a run restricted from the operator's key is refused with
// ErrOperatorKeyForbidden rather than sent on it.
//
// The registry hands drivers out wrapped, so reach this through As, never a
// type assertion.
type SpeechSynthesizer interface {
	SynthesizeSpeech(ctx context.Context, req SpeechRequest) (SpeechResponse, error)
}

// SpeechRequest is one synthesis request. Every value is the runtime's: the
// model and voice come from operator configuration (a model only picks among
// the configured voices), and the input has been normalised to speakable text.
type SpeechRequest struct {
	// Model is the concrete model id the endpoint serves.
	Model string
	// Voice is the engine's own voice id.
	Voice string
	// Input is the text to speak.
	Input string
	// Format is the audio the endpoint is asked for: "wav" or "pcm". The
	// runtime joins and encodes the pieces itself, so it never asks for more.
	Format string
	// Speed is the speaking rate; 0 is not sent. Ollama and Phonon's own
	// server refuse the field with a 400, so it goes out only when set.
	Speed float64
	// Instructions steer the delivery on models that take them; "" is not
	// sent, for the same reason as Speed.
	Instructions string
}

// SpeechResponse is the endpoint's audio as it came, bounded, and whose key
// paid. The bytes are not parsed: a streamed WAV from Ollama or Phonon's server
// carries 0xFFFFFFFF in its size fields, and the joiner, not the driver, takes
// the length from the bytes received.
type SpeechResponse struct {
	Audio []byte
	// ContentType is the endpoint's Content-Type, or the format's own type
	// when the endpoint sent none.
	ContentType string
	// CredentialSource is "operator", or the scope of the run's own
	// credential ("tenant", "user", "agent"); CredentialScopeID names its
	// owner.
	CredentialSource  string
	CredentialScopeID string
}

// MaxSpeechResponseBytes bounds the audio one piece may come back as: about
// twenty minutes of 24 kHz 16-bit mono WAV, far beyond what a piece of a few
// thousand characters produces. More is an error, never a silent cut.
const MaxSpeechResponseBytes = 64 << 20
