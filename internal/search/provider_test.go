package search

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captured records what a driver actually sent, so a test can assert the wire
// contract (method, auth header, params) as well as the normalized output.
type captured struct {
	method string
	header http.Header
	rawURL string
	body   string
}

func serve(t *testing.T, status int, body string, cap *captured) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cap != nil {
			cap.method = r.Method
			cap.header = r.Header.Clone()
			cap.rawURL = r.URL.String()
			b, _ := io.ReadAll(r.Body)
			cap.body = string(b)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDrivers_Normalize: each driver maps its provider's JSON to the common
// []Result{Title,URL,Snippet} shape and carries the resolved key on the wire.
func TestDrivers_Normalize(t *testing.T) {
	cases := []struct {
		name     string
		newP     func(url string) Provider
		resp     string
		wantID   string
		wantRes  []Result
		checkReq func(t *testing.T, c *captured)
	}{
		{
			name:    "brave",
			newP:    func(u string) Provider { return NewBrave(WithEndpoint(u)) },
			resp:    `{"web":{"results":[{"title":"T1","url":"https://a","description":"D1"},{"title":"T2","url":"https://b","description":"D2"}]}}`,
			wantID:  "brave",
			wantRes: []Result{{"T1", "https://a", "D1"}, {"T2", "https://b", "D2"}},
			checkReq: func(t *testing.T, c *captured) {
				if c.method != "GET" {
					t.Errorf("brave method = %s, want GET", c.method)
				}
				if c.header.Get("X-Subscription-Token") != "KEY" {
					t.Errorf("brave missing X-Subscription-Token; got %q", c.header.Get("X-Subscription-Token"))
				}
				if !strings.Contains(c.rawURL, "q=hi") || !strings.Contains(c.rawURL, "count=3") {
					t.Errorf("brave query = %q, want q=hi & count=3", c.rawURL)
				}
			},
		},
		{
			name:    "serper",
			newP:    func(u string) Provider { return NewSerper(WithEndpoint(u)) },
			resp:    `{"organic":[{"title":"T1","link":"https://a","snippet":"S1"}]}`,
			wantID:  "serper",
			wantRes: []Result{{"T1", "https://a", "S1"}},
			checkReq: func(t *testing.T, c *captured) {
				if c.method != "POST" {
					t.Errorf("serper method = %s, want POST", c.method)
				}
				if c.header.Get("X-API-KEY") != "KEY" {
					t.Errorf("serper missing X-API-KEY; got %q", c.header.Get("X-API-KEY"))
				}
				if !strings.Contains(c.body, `"q":"hi"`) {
					t.Errorf("serper body = %q, want q=hi", c.body)
				}
			},
		},
		{
			name:    "exa",
			newP:    func(u string) Provider { return NewExa(WithEndpoint(u)) },
			resp:    `{"results":[{"title":"T1","url":"https://a","text":"X1"},{"title":"T2","url":"https://b","highlights":["H2"]}]}`,
			wantID:  "exa",
			wantRes: []Result{{"T1", "https://a", "X1"}, {"T2", "https://b", "H2"}}, // text preferred; highlights fallback
			checkReq: func(t *testing.T, c *captured) {
				if c.header.Get("x-api-key") != "KEY" {
					t.Errorf("exa missing x-api-key; got %q", c.header.Get("x-api-key"))
				}
			},
		},
		{
			name:    "tavily",
			newP:    func(u string) Provider { return NewTavily(WithEndpoint(u)) },
			resp:    `{"results":[{"title":"T1","url":"https://a","content":"C1"}]}`,
			wantID:  "tavily",
			wantRes: []Result{{"T1", "https://a", "C1"}},
			checkReq: func(t *testing.T, c *captured) {
				if c.header.Get("Authorization") != "Bearer KEY" {
					t.Errorf("tavily Authorization = %q, want Bearer KEY", c.header.Get("Authorization"))
				}
			},
		},
		{
			name:    "searxng",
			newP:    func(u string) Provider { return NewSearXNG(u) },
			resp:    `{"results":[{"title":"T1","url":"https://a","content":"C1"}]}`,
			wantID:  "searxng",
			wantRes: []Result{{"T1", "https://a", "C1"}},
			checkReq: func(t *testing.T, c *captured) {
				if !strings.Contains(c.rawURL, "format=json") {
					t.Errorf("searxng query = %q, want format=json", c.rawURL)
				}
				if !strings.HasPrefix(c.rawURL, "/search") {
					t.Errorf("searxng path = %q, want /search", c.rawURL)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &captured{}
			srv := serve(t, 200, tc.resp, cap)
			p := tc.newP(srv.URL)
			if p.ID() != tc.wantID {
				t.Fatalf("ID() = %q, want %q", p.ID(), tc.wantID)
			}
			res, err := p.Search(context.Background(), Query{Text: "hi", MaxResults: 3}, "KEY")
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if res.Provider != tc.wantID {
				t.Errorf("Results.Provider = %q, want %q", res.Provider, tc.wantID)
			}
			if len(res.Results) != len(tc.wantRes) {
				t.Fatalf("got %d results, want %d (%+v)", len(res.Results), len(tc.wantRes), res.Results)
			}
			for i, want := range tc.wantRes {
				if res.Results[i] != want {
					t.Errorf("result[%d] = %+v, want %+v", i, res.Results[i], want)
				}
			}
			if len(res.Raw) == 0 {
				t.Error("Results.Raw should carry the original body")
			}
			tc.checkReq(t, cap)
		})
	}
}

// TestDrivers_ErrorPaths: a non-200 and malformed JSON both surface as errors so
// the WebSearch fallback circuit cascades to the next provider.
func TestDrivers_ErrorPaths(t *testing.T) {
	t.Run("non-200", func(t *testing.T) {
		srv := serve(t, 429, "rate limited", nil)
		_, err := NewBrave(WithEndpoint(srv.URL)).Search(context.Background(), Query{Text: "x", MaxResults: 1}, "K")
		if err == nil {
			t.Fatal("want error on 429")
		}
		if !strings.Contains(err.Error(), "429") {
			t.Errorf("error = %v, want it to mention 429", err)
		}
	})
	t.Run("bad-json", func(t *testing.T) {
		srv := serve(t, 200, "not json", nil)
		_, err := NewSerper(WithEndpoint(srv.URL)).Search(context.Background(), Query{Text: "x", MaxResults: 1}, "K")
		if err == nil {
			t.Fatal("want parse error on bad JSON")
		}
	})
}

// TestKeyEnvName: keyed providers advertise their env-var; SearXNG is keyless.
func TestKeyEnvName(t *testing.T) {
	want := map[string]string{
		"brave": "BRAVE_API_KEY", "serper": "SERPER_API_KEY", "exa": "EXA_API_KEY",
		"tavily": "TAVILY_API_KEY", "searxng": "",
	}
	ps := []Provider{NewBrave(), NewSerper(), NewExa(), NewTavily(), NewSearXNG("http://x")}
	for _, p := range ps {
		if got := p.KeyEnvName(); got != want[p.ID()] {
			t.Errorf("%s KeyEnvName() = %q, want %q", p.ID(), got, want[p.ID()])
		}
	}
}
