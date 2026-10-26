package transcribe

import (
	"fmt"
	"strings"
)

// ocrRank orders the modes: a call may run at or below its agent's.
var ocrRank = map[OCRMode]int{OCRNever: 0, OCRAuto: 1, OCRAlways: 2}

// ModeFor is the OCR mode one call runs with. The agent's mode (ceiling) is
// both its default and its limit: a call that names no mode runs the agent's,
// and one that names a mode may lower it, never raise it — OCR is billed per
// page, and only the agent's definition opts in to spending on it. An empty
// ceiling is never.
func ModeFor(ceiling OCRMode, requested string) (OCRMode, error) {
	if ceiling == "" {
		ceiling = OCRNever
	}
	if requested == "" {
		return ceiling, nil
	}
	m := OCRMode(requested)
	r, ok := ocrRank[m]
	if !ok {
		return "", fmt.Errorf("ocr %q is not one of never, auto, always", requested)
	}
	if r > ocrRank[ceiling] {
		return "", fmt.Errorf("ocr %q is above this agent's setting (%s)", requested, ceiling)
	}
	return m, nil
}

// defaultOCRPrompt is the built-in instruction (prompt_template replaces it).
// {language} is the sanitised hint.
const defaultOCRPrompt = "Transcribe all text in this document verbatim, as Markdown. " +
	"Keep the reading order. Write headings with #, tables as GitHub-flavoured Markdown tables, " +
	"and [illegible] for text you cannot read. Do not add commentary, do not summarise, " +
	"and do not describe pictures. Do not translate: keep each text in its own language. " +
	"The text is likely in these languages (ISO 639-1): {language}."

// OCRPrompt is the instruction for an OCR call: the operator's template, or
// the built-in one, with {language} replaced by the hint. Both inputs are
// trusted: the template is operator configuration and the language has been
// through SanitizeLanguage. Nothing a model wrote reaches it.
func OCRPrompt(template, language string) string {
	if strings.TrimSpace(template) == "" {
		template = defaultOCRPrompt
	}
	return strings.ReplaceAll(template, "{language}", strings.ReplaceAll(language, "+", ", "))
}
