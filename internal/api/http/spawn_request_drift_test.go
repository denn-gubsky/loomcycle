package http

import (
	"context"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
)

// fillNonZero sets every exported field of the struct v points to a non-zero
// value of its type, one level deep: enough for the mapper below to show
// whether it carries each field, without constructing valid nested values.
func fillNonZero(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int32, reflect.Int64:
			f.SetInt(1)
		case reflect.Ptr:
			// A pointer to true / 1, not to the zero value: a mapper that reads
			// *p (Review) would otherwise look like it dropped the field.
			p := reflect.New(f.Type().Elem())
			switch p.Elem().Kind() {
			case reflect.Bool:
				p.Elem().SetBool(true)
			case reflect.Int:
				p.Elem().SetInt(1)
			}
			f.Set(p)
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		case reflect.Map:
			m := reflect.MakeMap(f.Type())
			m.SetMapIndex(reflect.New(f.Type().Key()).Elem(), reflect.New(f.Type().Elem()).Elem())
			f.Set(m)
		default:
			t.Fatalf("fillNonZero: unhandled kind %s for %s", f.Kind(), v.Type().Field(i).Name)
		}
	}
}

// spawnRequestToRunInput is the one mapping from a spawn request to a run: MCP
// spawn_run, the gRPC batch and the start of every configured run go through
// it. It dropped Interruption, so a draft's interruption override (and MCP
// spawn_run's) was discarded at start.
//
// The drift guard: every field of connector.SpawnRunRequest must reach the
// same-named RunInput field, or be named below with the reason it does not.
func TestSpawnRequestToRunInput_CarriesEverySpawnField(t *testing.T) {
	var req connector.SpawnRunRequest
	fillNonZero(t, reflect.ValueOf(&req).Elem())
	got := reflect.ValueOf(spawnRequestToRunInput(req))

	notMapped := map[string]string{
		"Interactive": "a blocking spawn cannot park; the configured-run start sets RunInput.Interactive itself",
	}
	typ := reflect.TypeOf(req)
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if _, ok := notMapped[name]; ok {
			continue
		}
		f := got.FieldByName(name)
		if !f.IsValid() {
			t.Errorf("SpawnRunRequest.%s has no RunInput field of that name — map it or list it in notMapped", name)
			continue
		}
		if f.IsZero() {
			t.Errorf("spawnRequestToRunInput drops SpawnRunRequest.%s", name)
		}
	}
}

// The draft path end to end: a draft's interruption override — created over
// gRPC or MCP, which carry the field — is what its start hands the run.
func TestConfiguredRun_StartKeepsTheDraftsInterruption(t *testing.T) {
	srv, _, _, _ := configuredServer(t, 4)
	ctx := context.Background()
	c, err := srv.CreateConfiguredRun(ctx, connector.ConfiguredRunRequest{SpawnRunRequest: connector.SpawnRunRequest{
		Agent:        "agent",
		Segments:     []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		Interruption: &config.AgentInterruptionACL{Enabled: true, MaxPending: 2},
	}})
	if err != nil {
		t.Fatalf("CreateConfiguredRun: %v", err)
	}
	in, err := srv.ConfiguredRunInput(ctx, c.RunID, connector.RunSecrets{})
	if err != nil {
		t.Fatalf("ConfiguredRunInput: %v", err)
	}
	if in.Interruption == nil || !in.Interruption.Enabled || in.Interruption.MaxPending != 2 {
		t.Errorf("start input interruption = %+v, want the draft's", in.Interruption)
	}
}
