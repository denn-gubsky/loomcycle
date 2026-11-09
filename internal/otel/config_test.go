package otel

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestResolveConfig_EnvironmentTable pins what each OTEL_* / LOOMCYCLE_OTEL_*
// value turns into, and which values stop the boot.
func TestResolveConfig_EnvironmentTable(t *testing.T) {
	const ep = "http://collector:4318"
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string // substring; "" = no error
		check   func(t *testing.T, c Config)
	}{
		{name: "nothing set is off with defaults", env: nil, check: func(t *testing.T, c Config) {
			if c.Enabled {
				t.Error("Enabled with no endpoint")
			}
			if c.Protocol != ProtocolHTTPProtobuf || c.SamplerName != "parentbased_always_on" || c.SamplerArg != 1 || c.Inbound != InboundParent {
				t.Errorf("defaults = %+v", c)
			}
			if !reflect.DeepEqual(c.Propagators, []string{"tracecontext", "baggage"}) {
				t.Errorf("propagators = %v", c.Propagators)
			}
			if c.MaxQueueSize != 2048 || c.MaxExportBatchSize != 512 || c.ExportTimeout != 10*time.Second || c.ScheduleDelay != 5*time.Second {
				t.Errorf("batch defaults = %d/%d/%v/%v", c.MaxQueueSize, c.MaxExportBatchSize, c.ExportTimeout, c.ScheduleDelay)
			}
		}},
		{name: "endpoint enables", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ep}, check: wantEnabled(true)},
		{name: "traces endpoint enables", env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": ep + "/v1/traces"}, check: wantEnabled(true)},
		{name: "retired loomcycle endpoint is not read", env: map[string]string{"LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT": ep}, check: wantEnabled(false)},
		{name: "exporter none disables", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_TRACES_EXPORTER": "none"}, check: wantEnabled(false)},
		{name: "exporter otlp keeps on", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_TRACES_EXPORTER": "otlp"}, check: wantEnabled(true)},
		{name: "sdk disabled", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_SDK_DISABLED": "TRUE"}, check: wantEnabled(false)},
		{name: "sdk disabled false keeps on", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_SDK_DISABLED": "false"}, check: wantEnabled(true)},
		{name: "unsupported exporter with otlp is warned and ignored", env: on("OTEL_TRACES_EXPORTER", "otlp,console"), check: func(t *testing.T, c Config) {
			if !c.Enabled || !warned(c, `"console"`) {
				t.Errorf("enabled=%v warnings=%v, want on with a console warning", c.Enabled, c.Warnings)
			}
		}},
		{name: "only unsupported exporters export nothing", env: on("OTEL_TRACES_EXPORTER", "console"), check: func(t *testing.T, c Config) {
			if c.Enabled || !warned(c, `"console"`) {
				t.Errorf("enabled=%v warnings=%v, want off with a warning", c.Enabled, c.Warnings)
			}
		}},
		{name: "tracing off ignores every bad OTEL value", env: map[string]string{
			"OTEL_TRACES_EXPORTER": "otlp,zipkin", "OTEL_PROPAGATORS": "b3", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/json",
			"OTEL_TRACES_SAMPLER": "bogus", "OTEL_TRACES_SAMPLER_ARG": "7", "OTEL_BSP_MAX_QUEUE_SIZE": "lots",
			"OTEL_EXPORTER_OTLP_HEADERS": "s3cr3t%zz", "LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE": "/f",
		}, check: wantEnabled(false)},
		{name: "sdk disabled ignores bad values", env: on("OTEL_SDK_DISABLED", "true", "OTEL_TRACES_SAMPLER", "bogus"), check: wantEnabled(false)},
		{name: "scheme-less endpoint refused", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "otel-collector:4318"}, wantErr: "must be a URL with scheme"},
		{name: "scheme-less traces endpoint refused", env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "collector/v1/traces"}, wantErr: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"},
		{name: "endpoint userinfo never quoted", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "user:s3cr3t@collector:4318"}, wantErr: `"…@collector:4318"`},
		{name: "ftp endpoint refused", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "ftp://collector"}, wantErr: "must be a URL with scheme"},

		{name: "grpc protocol", env: on("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc"), check: func(t *testing.T, c Config) {
			if c.Protocol != ProtocolGRPC {
				t.Errorf("protocol = %q", c.Protocol)
			}
		}},
		{name: "traces protocol wins", env: on("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf"), check: func(t *testing.T, c Config) {
			if c.Protocol != ProtocolHTTPProtobuf {
				t.Errorf("protocol = %q", c.Protocol)
			}
		}},
		{name: "http/json refused", env: on("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json"), wantErr: `OTEL_EXPORTER_OTLP_PROTOCOL="http/json"`},
		{name: "bad traces protocol named", env: on("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "thrift"), wantErr: "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"},

		{name: "ratio sampler", env: on("OTEL_TRACES_SAMPLER", "parentbased_traceidratio", "OTEL_TRACES_SAMPLER_ARG", "0.25"), check: func(t *testing.T, c Config) {
			if c.SamplerName != "parentbased_traceidratio" || c.SamplerArg != 0.25 {
				t.Errorf("sampler = %s/%v", c.SamplerName, c.SamplerArg)
			}
		}},
		{name: "always_off sampler", env: on("OTEL_TRACES_SAMPLER", "always_off"), check: func(t *testing.T, c Config) {
			if c.SamplerName != "always_off" {
				t.Errorf("sampler = %s", c.SamplerName)
			}
		}},
		{name: "unknown sampler", env: on("OTEL_TRACES_SAMPLER", "jaeger_remote"), wantErr: `OTEL_TRACES_SAMPLER="jaeger_remote"`},
		{name: "arg above 1", env: on("OTEL_TRACES_SAMPLER_ARG", "1.5"), wantErr: "OTEL_TRACES_SAMPLER_ARG"},
		{name: "arg negative", env: on("OTEL_TRACES_SAMPLER_ARG", "-0.1"), wantErr: "OTEL_TRACES_SAMPLER_ARG"},
		{name: "arg not a number", env: on("OTEL_TRACES_SAMPLER_ARG", "half"), wantErr: "OTEL_TRACES_SAMPLER_ARG"},

		{name: "headers file alone", env: map[string]string{"LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE": "/run/secrets/otel"}, check: func(t *testing.T, c Config) {
			if c.HeadersFile != "/run/secrets/otel" {
				t.Errorf("headers file = %q", c.HeadersFile)
			}
		}},
		{name: "headers file with env headers", env: on("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE", "/f", "OTEL_EXPORTER_OTLP_HEADERS", "a=b"), wantErr: "OTEL_EXPORTER_OTLP_HEADERS are both set"},
		{name: "headers file with traces headers", env: on("LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE", "/f", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "a=b"), wantErr: "OTEL_EXPORTER_OTLP_TRACES_HEADERS are both set"},

		{name: "inbound link", env: map[string]string{"LOOMCYCLE_OTEL_INBOUND_CONTEXT": "link"}, check: wantInbound(InboundLink)},
		{name: "inbound ignore", env: map[string]string{"LOOMCYCLE_OTEL_INBOUND_CONTEXT": "ignore"}, check: wantInbound(InboundIgnore)},
		{name: "inbound parent", env: map[string]string{"LOOMCYCLE_OTEL_INBOUND_CONTEXT": "parent"}, check: wantInbound(InboundParent)},
		{name: "inbound bad", env: map[string]string{"LOOMCYCLE_OTEL_INBOUND_CONTEXT": "child"}, wantErr: "LOOMCYCLE_OTEL_INBOUND_CONTEXT"},

		{name: "propagators none", env: map[string]string{"OTEL_PROPAGATORS": "none"}, check: func(t *testing.T, c Config) {
			if len(c.Propagators) != 0 {
				t.Errorf("propagators = %v", c.Propagators)
			}
		}},
		{name: "propagators tracecontext only", env: map[string]string{"OTEL_PROPAGATORS": "tracecontext"}, check: func(t *testing.T, c Config) {
			if !reflect.DeepEqual(c.Propagators, []string{"tracecontext"}) {
				t.Errorf("propagators = %v", c.Propagators)
			}
		}},
		{name: "propagators b3 refused", env: on("OTEL_PROPAGATORS", "tracecontext,b3"), check: func(t *testing.T, c Config) {
			if !reflect.DeepEqual(c.Propagators, []string{"tracecontext"}) || !warned(c, `"b3"`) {
				t.Errorf("propagators = %v warnings = %v, want b3 ignored with a warning", c.Propagators, c.Warnings)
			}
		}},
		{name: "propagators none combined refused", env: on("OTEL_PROPAGATORS", "none,baggage"), check: func(t *testing.T, c Config) {
			if !reflect.DeepEqual(c.Propagators, []string{"baggage"}) || !warned(c, "none") {
				t.Errorf("propagators = %v warnings = %v", c.Propagators, c.Warnings)
			}
		}},

		{name: "bsp overrides", env: on("OTEL_BSP_MAX_QUEUE_SIZE", "100", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE", "500", "OTEL_BSP_EXPORT_TIMEOUT", "2500", "OTEL_BSP_SCHEDULE_DELAY", "200"), check: func(t *testing.T, c Config) {
			if c.MaxQueueSize != 100 || c.MaxExportBatchSize != 100 || c.ExportTimeout != 2500*time.Millisecond || c.ScheduleDelay != 200*time.Millisecond {
				t.Errorf("batch = %d/%d/%v/%v", c.MaxQueueSize, c.MaxExportBatchSize, c.ExportTimeout, c.ScheduleDelay)
			}
		}},
		{name: "bsp bad queue", env: on("OTEL_BSP_MAX_QUEUE_SIZE", "lots"), wantErr: "OTEL_BSP_MAX_QUEUE_SIZE"},
		{name: "bsp zero timeout", env: on("OTEL_BSP_EXPORT_TIMEOUT", "0"), wantErr: "OTEL_BSP_EXPORT_TIMEOUT"},

		{name: "malformed env header refused without its value", env: on("OTEL_EXPORTER_OTLP_HEADERS", "a=b,authorization=s3cr3t%zz"), wantErr: "OTEL_EXPORTER_OTLP_HEADERS: entry 2 has a malformed %-escape"},
		{name: "header without = refused", env: on("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "s3cr3t-token"), wantErr: "OTEL_EXPORTER_OTLP_TRACES_HEADERS: entry 1 is not key=value"},
		{name: "invalid header name refused", env: on("OTEL_EXPORTER_OTLP_HEADERS", "bad key=s3cr3t"), wantErr: "entry 1 has an invalid header name"},
		{name: "inbound bad even with tracing off", env: map[string]string{"LOOMCYCLE_OTEL_INBOUND_CONTEXT": "sideways"}, wantErr: "LOOMCYCLE_OTEL_INBOUND_CONTEXT"},
		{name: "environment", env: map[string]string{"LOOMCYCLE_ENVIRONMENT": "staging"}, check: func(t *testing.T, c Config) {
			if c.Environment != "staging" {
				t.Errorf("environment = %q", c.Environment)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ResolveConfig(envOf(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "s3cr3t") {
					t.Errorf("error quotes a header value: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			tc.check(t, c)
		})
	}
}

// on is an environment with tracing enabled plus the given key/value pairs.
func on(kv ...string) map[string]string {
	m := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func warned(c Config, substr string) bool {
	for _, w := range c.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func wantEnabled(want bool) func(*testing.T, Config) {
	return func(t *testing.T, c Config) {
		if c.Enabled != want {
			t.Errorf("Enabled = %v, want %v", c.Enabled, want)
		}
	}
}

func wantInbound(want InboundMode) func(*testing.T, Config) {
	return func(t *testing.T, c Config) {
		if c.Inbound != want {
			t.Errorf("Inbound = %q, want %q", c.Inbound, want)
		}
	}
}

// TestReadHeadersFile_ParsesPairsAndLines covers both layouts the file may
// use, comments, blank lines and the spec's percent-encoding.
func TestReadHeadersFile_ParsesPairsAndLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headers")
	body := "# collector auth\n\nauthorization = Basic%20dXNlcjpwYXNz\nx-scope-orgid=tenant-a, x-extra=1\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadHeadersFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"authorization": "Basic dXNlcjpwYXNz", "x-scope-orgid": "tenant-a", "x-extra": "1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("headers = %v, want %v", got, want)
	}
}

// TestReadHeadersFile_ErrorsNeverQuoteAValue: a malformed file stops the boot,
// and the error names the line or key, never a secret value.
func TestReadHeadersFile_ErrorsNeverQuoteAValue(t *testing.T) {
	if _, err := ReadHeadersFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file: want an error")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{
		"no-equals":  "s3cr3t-token\n",
		"bad-escape": "authorization=s3cr3t%zz\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadHeadersFile(path)
		if err == nil {
			t.Errorf("%s: want an error", name)
			continue
		}
		if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: error quotes the value: %v", name, err)
		}
	}
}
