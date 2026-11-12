package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// storeSweeperStepOrder is the order startStoreSweepers starts its sweepers
// in (see the comment above startStoreSweepers).
var storeSweeperStepOrder = []string{
	"expired_token", "ui_session", "auth_gate", "auth_lifecycle", "memory",
	"memory_pending", "memory_changes", "change_subscriptions", "deadlink",
	"document_boot_pass", "projection", "channels", "interrupts",
}

// WHY: the sweepers start in the order the boot log has always shown them,
// and the document boot pass starts after the dead-link GC goroutine. A swap
// of two steps compiles and passes every other test.
func TestStartStoreSweepers_StepsRunInOrder(t *testing.T) {
	b := &boot{cfg: &config.Config{}}
	var got []string
	b.runStoreSweeperSteps(func(step string) { got = append(got, step) })

	if !slices.Equal(got, storeSweeperStepOrder) {
		t.Errorf("startStoreSweepers step order\n got: %s\nwant: %s", strings.Join(got, " > "), strings.Join(storeSweeperStepOrder, " > "))
	}
}
