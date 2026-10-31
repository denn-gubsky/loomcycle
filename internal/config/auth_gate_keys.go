package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A gate entry is decoded strictly: an unknown key fails the load. A gate key
// is a security setting, and a misspelled one, or one this version does not
// know yet (a config written for a newer runtime), must never be dropped in
// silence: new_user.status: pending ignored by a runtime that cannot enforce
// it would let every website sign-up in without approval.

// UnmarshalYAML refuses a key no gate kind takes.
func (g *AuthGateConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := strictInlineKeys(n, "auth.gates entry", reflect.TypeOf(*g)); err != nil {
		return err
	}
	type plain AuthGateConfig
	return n.Decode((*plain)(g))
}

// UnmarshalYAML refuses a key the provisioning floor does not take.
func (u *AuthGateNewUserConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := strictInlineKeys(n, "auth.gates entry new_user", reflect.TypeOf(*u)); err != nil {
		return err
	}
	type plain AuthGateNewUserConfig
	return n.Decode((*plain)(u))
}

// strictInlineKeys is strictKeys for a struct with `yaml:",inline"` fields:
// the keys of an inlined struct are the outer block's keys.
func strictInlineKeys(n *yaml.Node, place string, t reflect.Type) error {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	known := map[string]bool{}
	yamlKeysOf(t, known)
	for i := 0; i+1 < len(n.Content); i += 2 {
		if key := n.Content[i].Value; !known[key] {
			names := make([]string, 0, len(known))
			for k := range known {
				names = append(names, k)
			}
			sort.Strings(names)
			return fmt.Errorf("%s: unknown key %q (it takes: %s)", place, key, strings.Join(names, ", "))
		}
	}
	return nil
}

func yamlKeysOf(t reflect.Type, into map[string]bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if name == "" && strings.Contains(opts, "inline") && f.Type.Kind() == reflect.Struct {
			yamlKeysOf(f.Type, into)
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		into[name] = true
	}
}
