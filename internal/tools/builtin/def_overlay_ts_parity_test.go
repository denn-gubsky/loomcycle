package builtin

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
)

// The typed HookDef / ScheduleDef / WebhookDef inputs in adapters/ts/src/types.ts
// are hand-written mirrors of the Go shapes the server decodes them into. Both
// directions matter:
//
//   - a Go field missing from TS is a field a typed caller cannot set — and on
//     HookDefOverlay, which is closed because the server decodes it strictly,
//     cannot set AT ALL;
//   - a TS field missing from Go is worse: on HookDef the server refuses it, on
//     ScheduleDef / WebhookDef it is silently dropped, so the caller believes
//     they configured something they did not.
//
// The field lists are read from both sources (reflection here, a scan of the TS
// declaration there); the only list kept in this file is the server-only
// register, and TestDefOverlayServerOnlyFields_OverlayCannotSetThem proves each
// of its entries really is out of an overlay's reach.

// serverOnlyOverlayFields are Go fields an overlay can neither set nor clear:
// the server stamps them after merging the overlay, so the TS overlay leaves
// them out on purpose.
var serverOnlyOverlayFields = map[string]map[string]string{
	"ScheduleDefOverlay": {
		"operator_key_restricted": "captured from the authoring principal at create/fork (RFC AX)",
		"isolated":                "captured from the authoring principal at create/fork (RFC BX)",
		"capture_disabled":        "written only by a snapshot restore; cleared by re-supplying credentials",
		"operator_layer":          "stamped from the caller's operator authority after the overlay",
	},
	"WebhookDefOverlay": {
		"operator_key_restricted": "captured from the authoring principal at create/fork (RFC AX)",
		"isolated":                "captured from the authoring principal at create/fork (RFC BX)",
		"capture_disabled":        "written only by a snapshot restore; cleared by re-supplying credentials",
	},
}

func TestDefOverlayTSMirror_MatchesGoShapes(t *testing.T) {
	src, err := os.ReadFile("../../../adapters/ts/src/types.ts")
	if err != nil {
		t.Skipf("TS adapter not present: %v", err)
	}
	ts := string(src)

	pairs := []struct {
		tsName string
		goType reflect.Type
	}{
		{"HookDefOverlay", reflect.TypeOf(hooks.Def{})},
		{"HookDefMatch", reflect.TypeOf(hooks.DefMatch{})},
		{"HookDefBody", reflect.TypeOf(hooks.DefBody{})},
		{"HookDefInput", reflect.TypeOf(hookDefInput{})},
		{"ScheduleDefOverlay", reflect.TypeOf(mergedScheduleDef{})},
		{"ScheduledRunPromptSegment", reflect.TypeOf(mergedSchedulePromptSeg{})},
		{"ScheduledRunPromptContent", reflect.TypeOf(mergedSchedulePromptSegContent{})},
		{"ScheduledRunHook", reflect.TypeOf(mergedScheduleHook{})},
		// The webhook's on_complete decodes into the config type, not the
		// schedule's mirror; the one TS type has to fit both.
		{"ScheduledRunHook", reflect.TypeOf(config.ScheduledRunHook{})},
		{"ScheduleDefInput", reflect.TypeOf(scheduleDefInput{})},
		{"WebhookDefOverlay", reflect.TypeOf(mergedWebhookDef{})},
		{"WebhookDefAuth", reflect.TypeOf(mergedWebhookAuth{})},
		{"WebhookDefRateLimit", reflect.TypeOf(mergedWebhookRateLimit{})},
		{"WebhookDefSyncResponse", reflect.TypeOf(mergedWebhookSyncResp{})},
		{"WebhookDefInput", reflect.TypeOf(webhookDefInput{})},
	}
	for _, p := range pairs {
		tsFields := tsDeclaredFields(t, ts, p.tsName)
		goFields := goDecodedFields(p.goType)
		serverOnly := serverOnlyOverlayFields[p.tsName]

		var missingInTS, unknownToGo, staleServerOnly []string
		for f := range goFields {
			if _, ok := serverOnly[f]; !ok && !tsFields[f] {
				missingInTS = append(missingInTS, f)
			}
		}
		for f := range tsFields {
			if !goFields[f] {
				unknownToGo = append(unknownToGo, f)
			}
		}
		for f := range serverOnly {
			if !goFields[f] || tsFields[f] {
				staleServerOnly = append(staleServerOnly, f)
			}
		}
		sort.Strings(missingInTS)
		sort.Strings(unknownToGo)
		sort.Strings(staleServerOnly)
		if len(missingInTS) > 0 {
			t.Errorf("%s (TS) lacks %s's fields %v — add them to adapters/ts/src/types.ts, or, if the "+
				"server stamps them, to serverOnlyOverlayFields with the reason", p.tsName, p.goType, missingInTS)
		}
		if len(unknownToGo) > 0 {
			t.Errorf("%s (TS) declares %v, which %s does not decode — the server would refuse or drop them",
				p.tsName, unknownToGo, p.goType)
		}
		if len(staleServerOnly) > 0 {
			t.Errorf("serverOnlyOverlayFields[%q] entries %v are no longer Go fields of %s, or are now "+
				"declared in TS — remove them so the register keeps meaning something", p.tsName, staleServerOnly, p.goType)
		}
	}
}

// TestDefOverlayServerOnlyFields_OverlayCannotSetThem backs each register entry
// with behaviour: an overlay carrying the field, merged onto an empty def, leaves
// it unset. Were applyOverlay to start copying one, it would belong in the TS
// overlay, and this fails.
func TestDefOverlayServerOnlyFields_OverlayCannotSetThem(t *testing.T) {
	check := func(t *testing.T, name string, ov reflect.Value, apply func() reflect.Value) {
		t.Helper()
		for field := range serverOnlyOverlayFields[name] {
			sf, ok := fieldByJSONName(ov.Type(), field)
			if !ok {
				t.Errorf("%s: no Go field %q", name, field)
				continue
			}
			setNonZero(t, ov.FieldByIndex(sf.Index))
		}
		got := apply()
		for field := range serverOnlyOverlayFields[name] {
			sf, _ := fieldByJSONName(got.Type(), field)
			if sf.Index != nil && !got.FieldByIndex(sf.Index).IsZero() {
				t.Errorf("%s: an overlay set %q — it is not server-only any more", name, field)
			}
		}
	}

	t.Run("ScheduleDef", func(t *testing.T) {
		var ov, def mergedScheduleDef
		check(t, "ScheduleDefOverlay", reflect.ValueOf(&ov).Elem(), func() reflect.Value {
			def.applyOverlay(ov)
			return reflect.ValueOf(def)
		})
	})
	t.Run("WebhookDef", func(t *testing.T) {
		var ov, def mergedWebhookDef
		check(t, "WebhookDefOverlay", reflect.ValueOf(&ov).Elem(), func() reflect.Value {
			def.applyOverlay(ov)
			return reflect.ValueOf(def)
		})
	})
}

// goDecodedFields is the set of JSON keys encoding/json decodes into t.
func goDecodedFields(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		if name, ok := jsonName(t.Field(i)); ok {
			out[name] = true
		}
	}
	return out
}

func fieldByJSONName(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		if n, ok := jsonName(t.Field(i)); ok && n == name {
			return t.Field(i), true
		}
	}
	return reflect.StructField{}, false
}

// jsonName is the key a field decodes from. A field with no json tag (the
// yaml-tagged config.ScheduledRunHook) decodes from its Go name, which
// encoding/json matches case-insensitively — so "kind" reaches Kind, and the
// lower-cased name is the key a caller writes.
func jsonName(f reflect.StructField) (string, bool) {
	if !f.IsExported() {
		return "", false
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	if name, _, _ := strings.Cut(tag, ","); name != "" {
		return name, true
	}
	return strings.ToLower(f.Name), true
}

func setNonZero(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.String:
		v.SetString("x")
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	default:
		t.Fatalf("setNonZero: no non-zero value for a %s field — extend it", v.Kind())
	}
}

// tsDeclaredFields returns the property names declared directly by the TS type
// `name` — `export type name = …;` or `export interface name {…}` — across every
// member of a union of object literals. Names in nested object types, index
// signatures and comments are not counted; named type references are not
// followed (each has its own pair).
func tsDeclaredFields(t *testing.T, src, name string) map[string]bool {
	t.Helper()
	start := -1
	for _, head := range []string{"export type " + name + " =", "export interface " + name + " {"} {
		if i := strings.Index(src, head); i >= 0 {
			start = i + len("export ")
			break
		}
	}
	if start < 0 {
		t.Fatalf("types.ts: no declaration of %s — renamed? This test asserts nothing until it finds it again", name)
	}
	decl := stripTSComments(src[start:])
	isInterface := strings.HasPrefix(decl, "interface ")
	if isInterface {
		decl = decl[strings.Index(decl, "{"):]
	} else {
		decl = decl[strings.Index(decl, "=")+1:]
	}

	fields := map[string]bool{}
	depth, square := 0, 0
	inString := byte(0)
	for i := 0; i < len(decl); i++ {
		c := decl[i]
		if inString != 0 {
			if c == '\\' {
				i++
			} else if c == inString {
				inString = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			inString = c
		case '{':
			depth++
		case '}':
			depth--
			if isInterface && depth == 0 {
				return nonEmpty(t, name, fields)
			}
		case '[':
			square++
		case ']':
			square--
		case ';':
			if !isInterface && depth == 0 {
				return nonEmpty(t, name, fields)
			}
		default:
			if depth != 1 || square != 0 || !isIdentStart(c) || (i > 0 && isIdentPart(decl[i-1])) {
				continue
			}
			j := i
			for j < len(decl) && isIdentPart(decl[j]) {
				j++
			}
			k := j
			if k < len(decl) && decl[k] == '?' {
				k++
			}
			for k < len(decl) && decl[k] == ' ' {
				k++
			}
			if k < len(decl) && decl[k] == ':' {
				fields[decl[i:j]] = true
			}
			i = j - 1
		}
	}
	t.Fatalf("types.ts: the declaration of %s never ends", name)
	return nil
}

func nonEmpty(t *testing.T, name string, fields map[string]bool) map[string]bool {
	t.Helper()
	if len(fields) == 0 {
		t.Fatalf("types.ts: read no fields from %s — the scan has stopped matching", name)
	}
	return fields
}

// stripTSComments drops // and /* */ comments, which carry prose such as
// "$cred:<name>" that the field scan would otherwise read as a property.
func stripTSComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 3
		case strings.HasPrefix(s[i:], "//"):
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				return b.String()
			}
			i += end - 1
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }
