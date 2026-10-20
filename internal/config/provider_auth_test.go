package config

import (
	"strings"
	"testing"
)

func bedrockProviderYAML(id, body string) string {
	return "providers:\n  " + id + ":\n" + body
}

func TestProviderAuth_ParsesAndValidates(t *testing.T) {
	cfg, err := loadYAMLString(t, bedrockProviderYAML("bedrock-claude",
		"    driver: anthropic\n"+
			"    base_url: https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic\n"+
			"    auth:\n"+
			"      type: aws_sigv4\n"+
			"      region: eu-central-1\n"+
			"      role_arn: arn:aws:iam::123456789012:role/loomcycle-bedrock\n"+
			"      external_id_env: LOOMCYCLE_BEDROCK_EXTERNAL_ID\n"+
			"      session_name: loomcycle-prod\n"))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Providers["bedrock-claude"].Auth
	if a == nil || a.Type != ProviderAuthSigV4 || a.Region != "eu-central-1" || a.RoleARN == "" || a.ExternalIDEnv == "" || a.SessionName != "loomcycle-prod" {
		t.Fatalf("auth = %+v", a)
	}
	if _, err := loadYAMLString(t, bedrockProviderYAML("bedrock-oss",
		"    driver: openai\n"+
			"    base_url: https://bedrock-mantle.us-east-1.api.aws/v1\n"+
			"    auth: {type: aws_bedrock_token, region: us-east-1}\n")); err != nil {
		t.Fatalf("mantle with a minted key: %v", err)
	}
}

func TestProviderAuth_RefusesWhatCannotWork(t *testing.T) {
	const runtime = "    base_url: https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic\n"
	cases := map[string]struct{ body, want string }{
		"no type":         {"    driver: anthropic\n" + runtime + "    auth: {region: eu-central-1}\n", "auth.type is required"},
		"unknown type":    {"    driver: anthropic\n" + runtime + "    auth: {type: aws_iam, region: eu-central-1}\n", `"aws_iam" is not`},
		"with a key":      {"    driver: anthropic\n" + runtime + "    api_key_env: AWS_BEARER_TOKEN_BEDROCK\n    auth: {type: aws_sigv4, region: eu-central-1}\n", "auth and api_key_env cannot both be set"},
		"other driver":    {"    driver: gemini\n" + runtime + "    auth: {type: aws_sigv4, region: eu-central-1}\n", "anthropic and openai drivers only"},
		"no base_url":     {"    driver: anthropic\n    auth: {type: aws_sigv4, region: eu-central-1}\n", "https base_url"},
		"plain http":      {"    driver: anthropic\n    base_url: http://bedrock-runtime.eu-central-1.amazonaws.com\n    auth: {type: aws_sigv4, region: eu-central-1}\n", "https base_url"},
		"no region":       {"    driver: anthropic\n" + runtime + "    auth: {type: aws_sigv4}\n", "auth.region is required"},
		"wrong region":    {"    driver: anthropic\n" + runtime + "    auth: {type: aws_sigv4, region: us-east-1}\n", `does not match base_url host`},
		"non-Bedrock":     {"    driver: openai\n    base_url: https://proxy.us-east-1.example.com/v1\n    auth: {type: aws_sigv4, region: us-east-1}\n", "auth.service is required"},
		"bad role":        {"    driver: anthropic\n" + runtime + "    auth: {type: aws_sigv4, region: eu-central-1, role_arn: arn:aws:iam::123:user/x}\n", "not an IAM role ARN"},
		"external no arn": {"    driver: anthropic\n" + runtime + "    auth: {type: aws_sigv4, region: eu-central-1, external_id_env: X}\n", "external_id_env needs role_arn"},
		"bad session":     {"    driver: anthropic\n" + runtime + "    auth: {type: aws_sigv4, region: eu-central-1, session_name: 'has space'}\n", "session_name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadYAMLString(t, bedrockProviderYAML("bedrock", tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A service name lets the generic block reach a non-Bedrock AWS endpoint
// (RFC EM D6: generic in shape, only Bedrock supported).
func TestProviderAuth_ExplicitServiceForAnotherHost(t *testing.T) {
	if _, err := loadYAMLString(t, bedrockProviderYAML("proxy",
		"    driver: openai\n    base_url: https://proxy.us-east-1.example.com/v1\n"+
			"    auth: {type: aws_sigv4, region: us-east-1, service: execute-api}\n")); err != nil {
		t.Fatal(err)
	}
}

// While a provider signs with loomcycle's AWS credentials, no allowlist may
// pass those credentials to a command an agent runs; without one, the same
// allowlist is left alone (it may carry AWS credentials meant to be shared).
func TestProviderAuth_RefusesAllowlistsThatLeakAWSSecrets(t *testing.T) {
	withAuth := bedrockProviderYAML("bedrock",
		"    driver: anthropic\n    base_url: https://bedrock-runtime.eu-central-1.amazonaws.com/anthropic\n"+
			"    auth: {type: aws_sigv4, region: eu-central-1}\n")
	for _, env := range []string{"LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV", "LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST", "LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "HOME, AWS_SESSION_TOKEN")
			if _, err := loadYAMLString(t, withAuth); err == nil || !strings.Contains(err.Error(), "AWS_SESSION_TOKEN") {
				t.Errorf("with auth: err = %v", err)
			}
			if _, err := loadYAMLString(t, "providers: {}\n"); err != nil {
				t.Errorf("without auth: %v", err)
			}
		})
	}
}
