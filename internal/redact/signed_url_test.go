package redact

import (
	"strings"
	"testing"
)

const signedMarker = "[redacted:signed-url]"

// jsonAmp is & as Go's JSON encoder writes it.
const jsonAmp = "\\u0026"

// A SharePoint upload session as ms365's create-upload-session returns it:
// anyone holding the URL can write the file without signing in.
const sharePointUploadURL = `https://contoso.sharepoint.com/sites/team/_api/v2.0/drives/b!FAKEdrive/items/01FAKEITEM/uploadSession?guid='0f1e2d3c-aaaa-bbbb-cccc-000000000000'&overwrite=False&rename=False&dc=0&tempauth=v1.eyJGAKEHEADER.eyJGAKEPAYLOADfake.FAKE_sig-0123`

const sharePointTempauth = `v1.eyJGAKEHEADER.eyJGAKEPAYLOADfake.FAKE_sig-0123`

// TestRedactor_SignedURLParamsMaskCredentialValues: the value of each
// credential-bearing query parameter is masked in every encoding a stored
// payload carries, and the rest of the URL is kept.
func TestRedactor_SignedURLParamsMaskCredentialValues(t *testing.T) {
	r := New(nil, false).WithSignedURLParams()
	cases := []struct {
		name   string
		in     string
		gone   []string
		stay   []string
		masked int
	}{
		{
			name:   "sharepoint tempauth in JSON text",
			in:     `{"uploadUrl":"` + sharePointUploadURL + `","expirationDateTime":"2026-10-24T12:15:00Z"}`,
			gone:   []string{sharePointTempauth},
			stay:   []string{`guid='0f1e2d3c-aaaa-bbbb-cccc-000000000000'&overwrite=False&rename=False&dc=0&tempauth=` + signedMarker + `","expirationDateTime"`},
			masked: 1,
		},
		{
			name:   "sharepoint tempauth with JSON-escaped separators",
			in:     strings.ReplaceAll(`{"uploadUrl":"`+sharePointUploadURL+`"}`, "&", jsonAmp),
			gone:   []string{sharePointTempauth},
			stay:   []string{"overwrite=False" + jsonAmp + "rename=False" + jsonAmp + "dc=0" + jsonAmp + "tempauth=" + signedMarker + `"}`},
			masked: 1,
		},
		{
			name:   "sharepoint tempauth inside escaped nested JSON",
			in:     `{\"uploadUrl\":\"` + sharePointUploadURL + `\",\"n\":1}`,
			gone:   []string{sharePointTempauth},
			stay:   []string{`&tempauth=` + signedMarker + `\",\"n\":1}`, `guid='0f1e2d3c`},
			masked: 1,
		},
		{
			name:   "html-escaped separators",
			in:     `<a href="https://x.sharepoint.com/f?dc=0&amp;tempauth=v1.FAKEhtml&amp;rename=False">`,
			gone:   []string{"v1.FAKEhtml"},
			stay:   []string{`dc=0&amp;tempauth=` + signedMarker + `&amp;rename=False">`},
			masked: 1,
		},
		{
			name: "s3 presigned",
			in:   `https://bucket.s3.amazonaws.com/k.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIDFAKEEXAMPLE%2F20261024%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20261024T000000Z&X-Amz-Expires=900&X-Amz-Security-Token=FAKEsession%2Btoken%3D&X-Amz-SignedHeaders=host&X-Amz-Signature=fake0123456789abcdef`,
			gone: []string{"AKIDFAKEEXAMPLE", "FAKEsession", "fake0123456789abcdef"},
			stay: []string{"X-Amz-Algorithm=AWS4-HMAC-SHA256&", "X-Amz-Date=20261024T000000Z&X-Amz-Expires=900&",
				"X-Amz-Security-Token=" + signedMarker + "&X-Amz-SignedHeaders=host&"},
			masked: 3,
		},
		{
			name:   "azure sas",
			in:     `https://acct.blob.core.windows.net/c/b.txt?sv=2022-11-02&sp=rw&se=2026-10-24T12:00:00Z&spr=https&sig=FAKEsig%2Babc%2Fdef%3D`,
			gone:   []string{"FAKEsig"},
			stay:   []string{"sv=2022-11-02&sp=rw&se=2026-10-24T12:00:00Z&spr=https&sig=" + signedMarker},
			masked: 1,
		},
		{
			name:   "gcs signed",
			in:     `https://storage.googleapis.com/b/o?X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Credential=fake%40proj.iam.gserviceaccount.com%2F20261024&X-Goog-Expires=900&X-Goog-Signature=fakeabcdef0123`,
			gone:   []string{"fake%40proj", "fakeabcdef0123"},
			stay:   []string{"X-Goog-Algorithm=GOOG4-RSA-SHA256&", "&X-Goog-Expires=900&"},
			masked: 2,
		},
		{
			name:   "cloudfront with key pair id",
			in:     `https://d111111abcdef8.cloudfront.net/i.jpg?Expires=1700000000&Signature=FAKEcf~sig__&Key-Pair-Id=KFAKEPAIR`,
			gone:   []string{"FAKEcf~sig__"},
			stay:   []string{"Expires=1700000000&Signature=" + signedMarker + "&Key-Pair-Id=KFAKEPAIR"},
			masked: 1,
		},
		{
			name:   "cloudfront with policy",
			in:     `https://d1.cloudfront.net/i.jpg?Policy=eyJGAKEpolicy&Signature=FAKEcfpolicysig`,
			gone:   []string{"FAKEcfpolicysig"},
			stay:   []string{"Policy=eyJGAKEpolicy&Signature=" + signedMarker},
			masked: 1,
		},
		{
			name:   "access_token in a query and a fragment",
			in:     `https://graph.example.com/me?access_token=FAKEaccess1 then https://app.example.com/cb#access_token=FAKEaccess2&token_type=bearer`,
			gone:   []string{"FAKEaccess1", "FAKEaccess2"},
			stay:   []string{"&token_type=bearer"},
			masked: 2,
		},
		{
			name:   "token in a URL",
			in:     `{"download":"https://dl.example.com/f?token=FAKEdl123&name=a.txt"}`,
			gone:   []string{"FAKEdl123"},
			stay:   []string{"?token=" + signedMarker + "&name=a.txt\"}"},
			masked: 1,
		},
		{
			name:   "oauth code beside state",
			in:     `{"redirect":"https://app.example.com/callback?code=FAKEauthcode123&state=xyz789"}`,
			gone:   []string{"FAKEauthcode123"},
			stay:   []string{"?code=" + signedMarker + "&state=xyz789"},
			masked: 1,
		},
		{
			name:   "percent-encoded URL nested in another URL",
			in:     `https://login.example.com/r?next=https%3A%2F%2Facct.blob.core.windows.net%2Fc%2Fb%3Fsv%3D2022%26sig%3DFAKEsig%252Babc%26se%3D2026`,
			gone:   []string{"FAKEsig"},
			stay:   []string{"%3Fsv%3D2022%26sig%3D" + signedMarker + "%26se%3D2026"},
			masked: 1,
		},
		{
			name:   "parameter names match case-insensitively",
			in:     `https://x.example.com/f?TEMPAUTH=FAKEupper&x-amz-signature=fakelower`,
			gone:   []string{"FAKEupper", "fakelower"},
			masked: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.String(tc.in)
			for _, g := range tc.gone {
				if strings.Contains(got, g) {
					t.Errorf("credential %q survived: %s", g, got)
				}
			}
			for _, s := range tc.stay {
				if !strings.Contains(got, s) {
					t.Errorf("want %q kept; got %s", s, got)
				}
			}
			if n := strings.Count(got, signedMarker); n != tc.masked {
				t.Errorf("masked %d values, want %d: %s", n, tc.masked, got)
			}
			if again := r.String(got); again != got {
				t.Errorf("not idempotent:\n first  %s\n second %s", got, again)
			}
		})
	}
}

// TestRedactor_SignedURLParamsLeaveOrdinaryTextAlone: generic names are
// judged in a URL query only, and code and Signature only beside the
// parameter that makes them a credential.
func TestRedactor_SignedURLParamsLeaveOrdinaryTextAlone(t *testing.T) {
	r := New(nil, false).WithSignedURLParams()
	for _, in := range []string{
		`https://example.com/search?q=golang&page=2&sort=desc`,
		`the token is refreshed hourly; pass the code to the reviewer`,
		`set token=abc and sig=def in the form`,
		`https://example.com/lookup?code=US&country=1`,
		`https://example.com/verify?Signature=abc123&user=7`,
		`https://example.com/a?signature_version=4&mysig=x&x-sig=y&tokens=3&codec=h264`,
		`{"url":"https://example.com/a?b=c","code":"FAKE-not-a-url"}`,
	} {
		if got := r.String(in); got != in {
			t.Errorf("changed ordinary text:\n in  %s\n got %s", in, got)
		}
	}
}

// TestRedactor_SignedURLParamsOffUnlessAsked: New never enables the tier, so
// the outbound redact context plugin, which builds its redactor with New,
// leaves a signed URL usable for the model that must pass it on.
func TestRedactor_SignedURLParamsOffUnlessAsked(t *testing.T) {
	if got := New(nil, true).String(sharePointUploadURL); !strings.Contains(got, sharePointTempauth) {
		t.Errorf("New(nil, true) masked a signed URL: %s", got)
	}
	if !New(nil, false).WithSignedURLParams().Enabled() {
		t.Error("a redactor with only the signed-URL tier reports disabled")
	}
	var nilR *Redactor
	if nilR.WithSignedURLParams() != nil {
		t.Error("WithSignedURLParams on nil must stay nil")
	}
}

// TestRedactor_SignedURLParamsKeepFollowingParamsUnderPatterns: with the
// pattern tier also on (the server's at-rest redactor), the key=value rule
// does not swallow the parameters after a masked token.
func TestRedactor_SignedURLParamsKeepFollowingParamsUnderPatterns(t *testing.T) {
	r := New(nil, true).WithSignedURLParams()
	in := `https://b.s3.amazonaws.com/k?X-Amz-Security-Token=FAKEsess&X-Amz-SignedHeaders=host&X-Amz-Expires=900`
	got := r.String(in)
	if strings.Contains(got, "FAKEsess") {
		t.Errorf("token survived: %s", got)
	}
	if !strings.Contains(got, "&X-Amz-SignedHeaders=host&X-Amz-Expires=900") {
		t.Errorf("following params lost: %s", got)
	}
}

func BenchmarkRedactor_StringNoURL(b *testing.B) {
	r := New(map[string]string{"LOOMCYCLE_X_TOKEN": "abcdefgh12345678"}, true).WithSignedURLParams()
	s := strings.Repeat("The agent read the file and summarised it for the user. ", 40)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = r.String(s)
	}
}

func BenchmarkRedactor_StringSignedURL(b *testing.B) {
	r := New(map[string]string{"LOOMCYCLE_X_TOKEN": "abcdefgh12345678"}, true).WithSignedURLParams()
	s := strings.Repeat("The agent read the file and summarised it for the user. ", 20) +
		`{"uploadUrl":"` + sharePointUploadURL + `"}` + strings.Repeat(" more prose here ", 20)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = r.String(s)
	}
}

// TestRedactor_SignedURLParamsEveryNameInEveryEncoding: each listed name is
// masked after each separator a stored payload carries, so the prefilter in
// front of the regex never skips a string the regex would change.
func TestRedactor_SignedURLParamsEveryNameInEveryEncoding(t *testing.T) {
	r := New(nil, false).WithSignedURLParams()
	companion := map[string]string{"signature": "Key-Pair-Id", "code": "state"}
	for _, name := range signedNames {
		for _, enc := range []struct{ sep, eq string }{
			{"?", "="}, {"&", "="}, {"#", "="}, {"&amp;", "="}, {jsonAmp, "="}, {"%3F", "%3D"}, {"%26", "%3d"},
		} {
			for _, n := range []string{name, strings.ToUpper(name)} {
				in := "https://h.example.com/p" + enc.sep + n + enc.eq + "FAKEvalue1"
				if c, ok := companion[name]; ok {
					in += enc.sep + c + enc.eq + "x"
				}
				if got := r.String(in); strings.Contains(got, "FAKEvalue1") || !strings.Contains(got, n+enc.eq+signedMarker) {
					t.Errorf("%s after %q: got %s", n, enc.sep, got)
				}
			}
		}
	}
}
