package providers

import (
	"net/url"
	"strings"
)

// KeyEnvNameFor is the credential name a provider built from o keys by, on a
// driver whose built-in key name is vendorKeyName (OPENAI_API_KEY) and whose
// vendor API is vendorBaseURL (https://api.openai.com/v1). Every driver
// factory with a vendor key name decides it here, and the chat, speech and
// transcription calls all key by the result.
//
// A vendor's key name applies only when the provider talks to that vendor. The
// name does two things: it is the operator env var a config names, and a
// tenant's own credential stored under it overrides the operator's key. A
// provider that points the openai driver at a self-hosted server (whisper,
// vLLM, llama.cpp) and names no api_key_env would otherwise send each tenant's
// own OpenAI key to that server, possibly over plain HTTP, and book the call
// as tenant-paid. So with no configured name and an endpoint that is not the
// vendor's, the provider has no key name: no tenant credential is looked up and
// the call carries no key. An operator whose OpenAI-compatible proxy needs the
// OpenAI key names api_key_env explicitly.
func (o DriverOptions) KeyEnvNameFor(vendorKeyName, vendorBaseURL string) string {
	if o.KeyEnvName != "" || vendorKeyName == "" {
		return o.KeyEnvName
	}
	if IsVendorEndpoint(o.BaseURL, vendorBaseURL) {
		return vendorKeyName
	}
	if o.Logger != nil {
		// Info, not Warn: a keyless self-hosted endpoint is the normal case.
		// It is logged so an operator whose proxy needed the vendor key finds
		// why its calls now go out without one.
		o.Logger.Info("provider: endpoint is not the vendor's API, so it calls without a key; name api_key_env to send one",
			"provider", o.ID, "vendor_key_name", vendorKeyName, "host", endpointHost(o.BaseURL))
	}
	return ""
}

// IsVendorEndpoint reports whether baseURL is the vendor API vendorBaseURL
// names. An empty baseURL is the vendor: the driver then dials its own default.
// Otherwise the scheme, the host (case-insensitive, a trailing dot ignored) and
// the port (the scheme's default when absent) must all match; the path does
// not count. A vendor's regional or partner hosts are not the vendor here: a
// provider pointed at one names its api_key_env. Matching the host exactly,
// never by suffix, keeps "api.openai.com.example" and
// "api.openai.com@example" out.
func IsVendorEndpoint(baseURL, vendorBaseURL string) bool {
	if baseURL == "" {
		return true
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	v, err := url.Parse(vendorBaseURL)
	if err != nil {
		return false
	}
	return u.Host != "" &&
		strings.EqualFold(u.Scheme, v.Scheme) &&
		strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), v.Hostname()) &&
		effectivePort(u) == effectivePort(v)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// endpointHost is the host of a base URL for a log line: never the whole URL,
// which may carry userinfo or a query.
func endpointHost(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil {
		return u.Hostname()
	}
	return ""
}
