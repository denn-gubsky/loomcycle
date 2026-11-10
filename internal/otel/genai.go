package otel

import (
	"go.opentelemetry.io/otel/attribute"
	genai "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// GenAISemconvVersion is the OpenTelemetry semantic-conventions release the
// gen_ai.* attributes follow. Those conventions are still in development and
// rename keys between releases, so one release is pinned (the newest the
// pinned otel module generates gen_ai keys for) and the loomcycle.* names stay
// beside them: operators' existing queries keep working whatever a later
// release renames.
const GenAISemconvVersion = "1.41.0"

// genAIProviderNames maps a loomcycle provider id to the gen_ai.provider.name
// well-known value of the pinned release. An id with no well-known value
// (ollama, a self-hosted server, an operator-named instance) is reported as
// itself, which the convention allows.
var genAIProviderNames = map[string]string{
	"anthropic":           genai.GenAIProviderNameAnthropic.Value.AsString(),
	"anthropic-oauth-dev": genai.GenAIProviderNameAnthropic.Value.AsString(),
	"openai":              genai.GenAIProviderNameOpenAI.Value.AsString(),
	"deepseek":            genai.GenAIProviderNameDeepseek.Value.AsString(),
	"gemini":              genai.GenAIProviderNameGCPGemini.Value.AsString(),
	"bedrock":             genai.GenAIProviderNameAWSBedrock.Value.AsString(),
	"ollama-local":        "ollama",
}

// GenAIProviderName is gen_ai.provider.name for a loomcycle provider id.
func GenAIProviderName(provider string) string {
	if v, ok := genAIProviderNames[provider]; ok {
		return v
	}
	return provider
}

// The gen_ai keys loomcycle emits, from the pinned release.
var (
	genAIOperationInvokeAgent = genai.GenAIOperationNameInvokeAgent
	genAIOperationChat        = genai.GenAIOperationNameChat
	genAIOperationExecuteTool = genai.GenAIOperationNameExecuteTool
)

func genAIString(k attribute.Key, v string, kvs []attribute.KeyValue) []attribute.KeyValue {
	if v == "" {
		return kvs
	}
	return append(kvs, k.String(v))
}
