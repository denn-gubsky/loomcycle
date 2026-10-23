package main

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/scripts/ci/gomodules"
)

func TestGroupByModule_OneLinePerOwningModuleRootFirst(t *testing.T) {
	mods := []gomodules.Module{
		{Dir: ".", Path: "example.com/m"},
		{Dir: "internal/a", Path: "example.com/m/internal/a"},
		{Dir: "internal/b", Path: "example.com/m/internal/b"},
	}
	got, err := groupByModule(mods, []string{
		"example.com/m/internal/a/sub",
		"example.com/m/cmd/x",
		"example.com/m/internal/a",
		"example.com/m/internal/store",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		". example.com/m/cmd/x example.com/m/internal/store",
		"internal/a example.com/m/internal/a/sub example.com/m/internal/a",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	if _, err := groupByModule(mods, []string{"./internal/a"}); err == nil {
		t.Fatal("a relative pattern was grouped; want an error, -group takes import paths")
	}
}
