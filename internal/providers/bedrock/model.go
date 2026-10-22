// Package bedrock embeds text with Cohere Embed v3 and v4 on Amazon Bedrock,
// through InvokeModel on the bedrock-runtime host, authenticated by the AWS
// credentials of the provider the embedder names (RFC EV).
//
// It is an embedder only. Bedrock chat goes through the anthropic and openai
// drivers on the same host; embeddings have no OpenAI-compatible route there,
// so they need a protocol of their own.
package bedrock

import (
	"fmt"
	"regexp"
	"strings"
)

// family is a Cohere Embed generation. The two differ in request fields and in
// the output widths they serve.
type family int

const (
	familyV3 family = iota + 1
	familyV4
)

// baseModels are the Cohere Embed text models Bedrock serves, by base id.
var baseModels = map[string]family{
	"cohere.embed-multilingual-v3": familyV3,
	"cohere.embed-english-v3":      familyV3,
	"cohere.embed-v4:0":            familyV4,
}

// profilePrefix matches an inference-profile prefix: a geography (us., eu.,
// apac., us-gov.) or global. It is a pattern rather than a list because AWS
// adds geographies, and the base id after it is what is checked.
var profilePrefix = regexp.MustCompile(`^[a-z]{2,8}(-[a-z]{2,8})?\.`)

const supportedModels = "cohere.embed-v4:0, cohere.embed-multilingual-v3, cohere.embed-english-v3, each optionally behind a profile prefix such as eu. or global."

// parseModel returns the Cohere generation of a Bedrock model id: a base id or
// a geographic/global inference-profile id. An ARN is refused: an application
// inference profile's model cannot be read from its id.
func parseModel(id string) (family, error) {
	if strings.HasPrefix(id, "arn:") {
		return 0, fmt.Errorf("model %q: inference-profile ARNs are not supported; use a model id or a profile id such as eu.cohere.embed-v4:0", id)
	}
	base := id
	if !strings.HasPrefix(id, "cohere.") {
		if loc := profilePrefix.FindStringIndex(id); loc != nil {
			base = id[loc[1]:]
		}
	}
	f, ok := baseModels[base]
	if !ok {
		return 0, fmt.Errorf("model %q is not a Cohere Embed text model on Bedrock (supported: %s)", id, supportedModels)
	}
	return f, nil
}

// checkDimensions validates a requested output width for f. 0 keeps the
// model's own width.
func checkDimensions(model string, f family, dimensions int) error {
	switch f {
	case familyV4:
		switch dimensions {
		case 0, 256, 512, 1024, 1536:
			return nil
		}
		return fmt.Errorf("dimensions %d is not served by %s (allowed: 0 = 1536, 256, 512, 1024, 1536)", dimensions, model)
	default:
		if dimensions == 0 || dimensions == 1024 {
			return nil
		}
		return fmt.Errorf("dimensions %d is not served by %s, whose width is fixed (allowed: 0 or 1024)", dimensions, model)
	}
}

// nativeDimension is the width f returns for a requested width of dimensions.
func nativeDimension(f family, dimensions int) int {
	if f == familyV4 && dimensions > 0 {
		return dimensions
	}
	if f == familyV4 {
		return 1536
	}
	return 1024
}

// validate is the config-load check registered beside the constructor.
func validate(model string, dimensions int) error {
	f, err := parseModel(model)
	if err != nil {
		return err
	}
	return checkDimensions(model, f, dimensions)
}
