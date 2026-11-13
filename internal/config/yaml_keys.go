package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// yamlKeyNodes returns the value node of every key the yaml tree names, by
// its dotted path ("retention.chats.max_age"). A key whose value is null is
// not named: it decodes as if it were absent.
func yamlKeyNodes(root *yaml.Node) map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	collectYAMLKeys(root, "", out)
	return out
}

func collectYAMLKeys(n *yaml.Node, prefix string, out map[string]*yaml.Node) {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil || n.Kind != yaml.MappingNode {
		return
	}
	// A merge key ("<<") supplies keys the mapping's own keys replace, so the
	// merged mappings are read first.
	for _, merge := range []bool{true, false} {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			for v.Kind == yaml.AliasNode && v.Alias != nil {
				v = v.Alias
			}
			if isMerge := k.ShortTag() == "!!merge"; isMerge != merge {
				continue
			}
			if merge {
				collectYAMLKeys(v, prefix, out)
				for _, item := range v.Content {
					if v.Kind == yaml.SequenceNode {
						collectYAMLKeys(item, prefix, out)
					}
				}
				continue
			}
			if v.Kind == yaml.ScalarNode && v.ShortTag() == "!!null" {
				continue
			}
			path := k.Value
			if prefix != "" {
				path = prefix + "." + k.Value
			}
			out[path] = v
			collectYAMLKeys(v, path, out)
		}
	}
}

// humanDurationKeys is the dotted yaml path of every HumanDuration under
// Config, found by type so a duration added to any block is included.
func humanDurationKeys() map[string]bool {
	out := map[string]bool{}
	collectDurationKeys(reflect.TypeOf(Config{}), "", map[reflect.Type]bool{}, out)
	return out
}

// collectDurationKeys follows struct fields and pointers to structs. A map or
// a list of structs has no fixed path and is not followed: a duration inside
// one is checked only when it is decoded, without its key in the error.
func collectDurationKeys(t reflect.Type, prefix string, onPath map[reflect.Type]bool, out map[string]bool) {
	if onPath[t] {
		return
	}
	onPath[t] = true
	defer delete(onPath, t)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		path := prefix
		if !strings.Contains(opts, "inline") {
			if name == "" {
				name = strings.ToLower(f.Name)
			}
			path = name
			if prefix != "" {
				path = prefix + "." + name
			}
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case ft == reflect.TypeOf(HumanDuration(0)):
			out[path] = true
		case ft.Kind() == reflect.Struct:
			collectDurationKeys(ft, path, onPath, out)
		}
	}
}

// checkDurationKeys refuses a duration the yaml gives in a form that does not
// parse, naming its key. The decode would refuse it too, with a line number
// only, and that line is one of the merged document when layers are stacked.
func checkDurationKeys(nodes map[string]*yaml.Node) error {
	keys := make([]string, 0, len(nodes))
	durations := humanDurationKeys()
	for key := range nodes {
		if durations[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		n := nodes[key]
		if n.Kind != yaml.ScalarNode {
			return fmt.Errorf("%s: a duration is one value such as 30d or 12h", key)
		}
		if _, err := ParseHumanDuration(n.Value); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}
