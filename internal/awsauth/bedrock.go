package awsauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// maxBedrockTokenLifetime is the longest a short-term Bedrock key may live.
const maxBedrockTokenLifetime = 12 * time.Hour

// Hosts and their signing services. The runtime host serves the Anthropic,
// Chat Completions and Responses routes and signs as "bedrock"; mantle serves
// the same routes and signs as "bedrock-mantle".
const (
	runtimeHostPrefix = "bedrock-runtime."
	mantleHostPrefix  = "bedrock-mantle."
)

// DefaultService is the SigV4 service name for a Bedrock base URL, or "" when
// the host is not a Bedrock host (the operator must then name the service).
func DefaultService(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	switch h := u.Hostname(); {
	case strings.HasPrefix(h, runtimeHostPrefix):
		return "bedrock"
	case strings.HasPrefix(h, mantleHostPrefix):
		return "bedrock-mantle"
	}
	return ""
}

// HostHasRegion reports whether baseURL's host names region as a whole dot-
// separated label (bedrock-runtime.eu-central-1.amazonaws.com,
// bedrock-mantle.eu-central-1.api.aws). A mismatch is the most common SigV4
// failure, and AWS's answer to it does not say which side is wrong.
func HostHasRegion(baseURL, region string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || region == "" {
		return false
	}
	for _, label := range strings.Split(u.Hostname(), ".") {
		if label == region {
			return true
		}
	}
	return false
}

// MintBedrockToken builds a short-term Bedrock API key from c, exactly as
// AWS's aws-bedrock-token-generator libraries do: a presigned
// CallWithBearerToken request, without its scheme, plus "&Version=1",
// base64-encoded behind the "bedrock-api-key-" prefix. The key lives for the
// shorter of lifetime, 12 hours, and what remains of c's own session, and only
// in region. It returns the key and when it stops working.
func MintBedrockToken(c Credentials, region string, lifetime time.Duration, now time.Time) (string, time.Time, error) {
	if lifetime <= 0 || lifetime > maxBedrockTokenLifetime {
		lifetime = maxBedrockTokenLifetime
	}
	if c.CanExpire {
		if left := c.Expires.Sub(now); left < lifetime {
			lifetime = left
		}
	}
	if lifetime < time.Second {
		return "", time.Time{}, fmt.Errorf("%w: credentials expire before a key can be minted", ErrNoCredentials)
	}
	lifetime = lifetime.Truncate(time.Second)
	signed, err := PresignURL(http.MethodPost, "https://bedrock.amazonaws.com/?Action=CallWithBearerToken", c, region, "bedrock", lifetime, now)
	if err != nil {
		return "", time.Time{}, err
	}
	raw := strings.TrimPrefix(signed, "https://") + "&Version=1"
	return "bedrock-api-key-" + base64.StdEncoding.EncodeToString([]byte(raw)), now.Add(lifetime), nil
}

// IsExpiredCredentialAnswer reports whether an error response says the
// credentials or key it carried had expired, the one auth failure worth one
// retry with fresh credentials. Access denials are not: runtime's OpenAI route
// answers a denial with 401, so the status alone proves nothing.
func IsExpiredCredentialAnswer(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	b := strings.ToLower(string(body))
	return strings.Contains(b, "expiredtoken") ||
		strings.Contains(b, "security token included in the request is expired") ||
		(strings.Contains(b, "expired") && (strings.Contains(b, "token") || strings.Contains(b, "api key")))
}

// listModels lists the model ids a Bedrock host serves, for the routing probe
// (a model must be listed to be routed to). Bedrock does not serve /v1/models
// on its Anthropic route, so:
//   - mantle: its own GET /v1/models, signed as bedrock-mantle;
//   - runtime: the control plane's inference profiles and foundation models,
//     signed as bedrock. Runtime takes Claude only by inference-profile id.
//
// Listing always signs, in either mode, and needs the list permissions the
// docs name (bedrock:ListInferenceProfiles and bedrock:ListFoundationModels
// for runtime).
func (a *Auth) listModels(ctx context.Context) ([]string, error) {
	c, err := a.credentials(ctx)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(a.baseURL)
	if err != nil {
		return nil, err
	}
	switch h := u.Hostname(); {
	case strings.HasPrefix(h, mantleHostPrefix):
		var doc struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := a.getJSON(ctx, u.Scheme+"://"+u.Host+"/v1/models", c, "bedrock-mantle", &doc); err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(doc.Data))
		for _, m := range doc.Data {
			ids = append(ids, m.ID)
		}
		sort.Strings(ids)
		return ids, nil
	case strings.HasPrefix(h, runtimeHostPrefix):
		control := a.controlURL
		if control == "" {
			control = "https://bedrock." + a.region + ".amazonaws.com"
		}
		var ids []string
		next := ""
		for page := 0; page < 20; page++ { // 20 pages of 1000 is far past any account
			q := url.Values{"maxResults": {"1000"}}
			if next != "" {
				q.Set("nextToken", next)
			}
			var doc struct {
				Profiles []struct {
					ID string `json:"inferenceProfileId"`
				} `json:"inferenceProfileSummaries"`
				NextToken string `json:"nextToken"`
			}
			if err := a.getJSON(ctx, control+"/inference-profiles?"+q.Encode(), c, "bedrock", &doc); err != nil {
				return nil, err
			}
			for _, p := range doc.Profiles {
				ids = append(ids, p.ID)
			}
			if next = doc.NextToken; next == "" {
				break
			}
		}
		var fm struct {
			Models []struct {
				ID string `json:"modelId"`
			} `json:"modelSummaries"`
		}
		if err := a.getJSON(ctx, control+"/foundation-models", c, "bedrock", &fm); err != nil {
			return nil, err
		}
		for _, m := range fm.Models {
			ids = append(ids, m.ID)
		}
		sort.Strings(ids)
		return ids, nil
	}
	return nil, fmt.Errorf("model listing: %s is not a Bedrock host", u.Hostname())
}

func (a *Auth) getJSON(ctx context.Context, rawURL string, c Credentials, service string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	SignRequest(req, nil, c, a.region, service, a.now())
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("bedrock model listing: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("bedrock model listing: %s: status %d (%s)", req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return fmt.Errorf("bedrock model listing: %s: %w", req.URL.Path, err)
	}
	return nil
}
