package policy

import (
	"reflect"
	"sort"
	"testing"
)

func TestApply(t *testing.T) {
	available := []string{"Read", "Write", "Bash", "mcp__brave-search__brave_web_search", "mcp__brave-search__brave_local_search"}

	cases := []struct {
		name          string
		agentAllowed  []string
		callerAllowed []string
		want          []string
	}{
		{
			name:         "agent restricts to read-only",
			agentAllowed: []string{"Read"},
			want:         []string{"Read"},
		},
		{
			name:         "glob matches mcp prefix",
			agentAllowed: []string{"Read", "mcp__brave-search__*"},
			want:         []string{"Read", "mcp__brave-search__brave_web_search", "mcp__brave-search__brave_local_search"},
		},
		{
			name:          "caller narrows agent",
			agentAllowed:  []string{"Read", "Write", "Bash"},
			callerAllowed: []string{"Read"},
			want:          []string{"Read"},
		},
		{
			name:          "caller cannot expand beyond agent",
			agentAllowed:  []string{"Read"},
			callerAllowed: []string{"Read", "Bash"},
			want:          []string{"Read"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Apply(available, tc.agentAllowed, tc.callerAllowed)
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}
