package tools

import (
	"errors"
	"testing"
)

// Admission is all or none, counts per run, and a release frees one slot once.
func TestLiveChildren_AdmitsUpToTheLimitPerRunAndReleasesOnce(t *testing.T) {
	l := NewLiveChildren(func() int { return 3 })
	rel, err := l.Admit("r1", 2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.Admit("r1", 2)
	var lim *LiveChildLimitError
	if !errors.As(err, &lim) || lim.Alive != 2 || lim.Want != 2 || lim.Limit != 3 {
		t.Fatalf("err = %v, want a refusal naming 2 alive, 2 wanted, limit 3", err)
	}
	if l.Alive("r1") != 2 {
		t.Errorf("a refused admission changed the count: %d alive", l.Alive("r1"))
	}
	if _, err := l.Admit("r2", 3); err != nil {
		t.Errorf("another run's children counted against r1: %v", err)
	}
	rel[0]()
	rel[0]()
	if l.Alive("r1") != 1 {
		t.Errorf("after one release (twice) %d alive, want 1", l.Alive("r1"))
	}
	if _, err := l.Admit("r1", 2); err != nil {
		t.Errorf("freed slots not reusable: %v", err)
	}
}

// A nil registry, a run with no id, and a non-positive limit admit everything.
func TestLiveChildren_UnboundedCasesAdmitEverything(t *testing.T) {
	var nilReg *LiveChildren
	for name, admit := range map[string]func() error{
		"nil registry": func() error { _, err := nilReg.Admit("r", 100); return err },
		"no run id":    func() error { _, err := NewLiveChildren(func() int { return 1 }).Admit("", 100); return err },
		"no limit":     func() error { _, err := NewLiveChildren(func() int { return 0 }).Admit("r", 100); return err },
	} {
		if err := admit(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
