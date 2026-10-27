// Package speech holds the pure parts of text-to-speech: turning Markdown into
// text an engine can read aloud, cutting that text into pieces an engine
// accepts, joining the WAV audio the engine returns, choosing a language from
// the text's script, and the per-run caps and budget.
//
// Speech is a runtime subroutine, the mirror of transcription: the caller
// normalises the text (Normalize), picks a language (Detect), cuts it
// (Split), reserves the characters and estimated seconds against the run's
// Budget BEFORE any engine call, asks the engine for WAV one piece at a time,
// and joins the pieces (JoinWAV) with a pause at each paragraph end. No audio
// byte ever enters a model's context.
//
// The package is stdlib only and imports nothing else of this module: engines,
// providers, models, voices and the operator's caps are configuration the
// caller resolves and hands in.
package speech
