package awsauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdkv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

var testCreds = Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}

// AWS's published SigV4 test-suite case "get-vanilla".
func TestSignRequest_MatchesTheAWSTestSuiteGetVanilla(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	now, _ := time.Parse(amzDateFormat, "20150830T123600Z")
	SignRequest(req, nil, testCreds, "us-east-1", "service", now)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n %s\nwant\n %s", got, want)
	}
}

// The AWS SDK's signer is the reference for the cases the published vector
// does not cover: a body, a session token, a query string, and a path that
// needs encoding (a Bedrock model id carries ':').
func TestSignRequest_MatchesTheSDKSigner(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	cases := map[string]struct {
		url   string
		body  []byte
		creds Credentials
	}{
		"post body":         {"https://bedrock-runtime.us-east-1.amazonaws.com/anthropic/v1/messages", []byte(`{"model":"m","max_tokens":1}`), testCreds},
		"session token":     {"https://bedrock-mantle.eu-central-1.api.aws/v1/chat/completions", []byte(`{}`), withToken(testCreds)},
		"query":             {"https://bedrock.us-east-1.amazonaws.com/inference-profiles?maxResults=1000&nextToken=a%2Fb%3D%3D&typeEquals=SYSTEM_DEFINED", nil, testCreds},
		"path with a colon": {"https://bedrock-runtime.us-east-1.amazonaws.com/model/us.anthropic.claude-x-v1:0/invoke", []byte(`{}`), testCreds},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			region, service := "us-east-1", "bedrock"
			ours := newReq(t, tc.url, tc.body)
			SignRequest(ours, tc.body, tc.creds, region, service, now)

			ref := newReq(t, tc.url, tc.body)
			ref.Header.Set("X-Amz-Date", now.Format(amzDateFormat)) // so both sign the same header set
			// The SDK also signs Content-Length; SigV4 does not require it and
			// ours does not, since the transport sets it after signing.
			ref.ContentLength = 0
			sum := sha256.Sum256(tc.body)
			err := sdkv4.NewSigner().SignHTTP(context.Background(), sdkCreds(tc.creds), ref, hex.EncodeToString(sum[:]), service, region, now)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := ours.Header.Get("Authorization"), ref.Header.Get("Authorization"); got != want {
				t.Errorf("Authorization =\n %s\nSDK\n %s", got, want)
			}
		})
	}
}

func TestPresignURL_MatchesTheSDKPresigner(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	for name, c := range map[string]Credentials{"long-term": testCreds, "session token": withToken(testCreds)} {
		t.Run(name, func(t *testing.T) {
			raw := "https://bedrock.amazonaws.com/?Action=CallWithBearerToken"
			ours, err := PresignURL(http.MethodPost, raw, c, "eu-central-1", "bedrock", 12*time.Hour, now)
			if err != nil {
				t.Fatal(err)
			}

			ref, _ := http.NewRequest(http.MethodPost, raw+"&X-Amz-Expires=43200", nil)
			empty := sha256.Sum256(nil)
			want, _, err := sdkv4.NewSigner().PresignHTTP(context.Background(), sdkCreds(c), ref, hex.EncodeToString(empty[:]), "bedrock", "eu-central-1", now)
			if err != nil {
				t.Fatal(err)
			}
			gotQ, wantQ := mustQuery(t, ours), mustQuery(t, want)
			if gotQ.Encode() != wantQ.Encode() {
				t.Errorf("presigned query =\n %s\nSDK\n %s", gotQ.Encode(), wantQ.Encode())
			}
		})
	}
}

func newReq(t *testing.T, u string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body == nil {
		req.Method = http.MethodGet
		req.Body = nil
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func withToken(c Credentials) Credentials {
	c.SessionToken = "IQoJb3JpZ2luX2VjEXAMPLE/token+with=specials"
	return c
}

func sdkCreds(c Credentials) aws.Credentials {
	return aws.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}
