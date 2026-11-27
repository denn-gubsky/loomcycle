package connector

import (
	"errors"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func TestCheckInterruptAnswer_AcceptsWhatTheQuestionAllows(t *testing.T) {
	opts := []byte(`["Yes","No"]`)
	atCap := strings.Repeat("é", MaxInterruptOtherAnswerChars) // 2 bytes each: the cap counts characters
	cases := []struct {
		name   string
		row    store.InterruptRow
		answer string
		ok     bool
	}{
		{"free text, an answer", store.InterruptRow{}, "anything", true},
		{"free text, empty", store.InterruptRow{}, "", false},
		{"free text, empty list", store.InterruptRow{Options: []byte(`[]`)}, "", false},
		{"options, one of them", store.InterruptRow{Options: opts}, "No", true},
		{"options, outside them", store.InterruptRow{Options: opts}, "Maybe", false},
		{"options, case differs", store.InterruptRow{Options: opts}, "yes", false},
		{"allow_other, an option", store.InterruptRow{Options: opts, AllowOther: true}, "Yes", true},
		{"allow_other, own answer", store.InterruptRow{Options: opts, AllowOther: true}, "Maybe", true},
		{"allow_other, empty", store.InterruptRow{Options: opts, AllowOther: true}, "", false},
		{"allow_other, at the cap", store.InterruptRow{Options: opts, AllowOther: true}, atCap, true},
		{"allow_other, over the cap", store.InterruptRow{Options: opts, AllowOther: true}, atCap + "x", false},
		{"allow_other without options", store.InterruptRow{AllowOther: true}, "", false},
		{"unreadable options", store.InterruptRow{Options: []byte(`{"a":1}`), AllowOther: true}, "Yes", false},
	}
	for _, c := range cases {
		err := CheckInterruptAnswer(c.row, c.answer)
		if c.ok && err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrInterruptInvalidAnswer) {
			t.Errorf("%s: err = %v, want ErrInterruptInvalidAnswer", c.name, err)
		}
	}
}
