package main

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

func TestApplyAllowedToolsFilter(t *testing.T) {
	descs := []mcp.ToolDescriptor{
		{Name: "search"},
		{Name: "fetch"},
		{Name: "summarise"},
	}

	tests := []struct {
		name    string
		allowed []string
		want    []string // tool names
	}{
		{
			name:    "empty allowed = pass through",
			allowed: nil,
			want:    []string{"search", "fetch", "summarise"},
		},
		{
			name:    "exact match keeps named tools",
			allowed: []string{"search", "fetch"},
			want:    []string{"search", "fetch"},
		},
		{
			name:    "non-matching name dropped",
			allowed: []string{"nonexistent"},
			want:    []string{},
		},
		{
			name:    "duplicate in allowed is harmless",
			allowed: []string{"search", "search"},
			want:    []string{"search"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyAllowedToolsFilter(descs, tc.allowed)
			gotNames := make([]string, 0, len(got))
			for _, d := range got {
				gotNames = append(gotNames, d.Name)
			}
			if len(gotNames) == 0 {
				gotNames = []string{} // normalise nil vs empty
			}
			if !reflect.DeepEqual(gotNames, tc.want) {
				t.Errorf("got %v, want %v", gotNames, tc.want)
			}
		})
	}
}
