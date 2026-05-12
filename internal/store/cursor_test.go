package store

import (
	"strings"
	"testing"
	"time"
)

// EncodeChannelCursor → DecodeChannelCursor round-trips a real
// (visible_at, msg_id) tuple cleanly.
func TestChannelCursor_RoundTrip(t *testing.T) {
	visAt := time.Unix(0, 1747042420123456789).UTC()
	msgID := MintChannelMessageID(visAt)
	encoded := EncodeChannelCursor(visAt, msgID)
	gotVis, gotID, fromOldest, err := DecodeChannelCursor(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fromOldest {
		t.Error("fromOldest=true on a real cursor")
	}
	if gotVis.UnixNano() != visAt.UnixNano() {
		t.Errorf("visible_at = %v, want %v", gotVis, visAt)
	}
	if gotID != msgID {
		t.Errorf("msg_id = %q, want %q", gotID, msgID)
	}
}

func TestChannelCursor_FromOldestSentinels(t *testing.T) {
	for _, s := range []string{"", "cur_0"} {
		_, _, fromOldest, err := DecodeChannelCursor(s)
		if err != nil {
			t.Errorf("decode %q: unexpected error %v", s, err)
		}
		if !fromOldest {
			t.Errorf("decode %q: fromOldest = false, want true", s)
		}
	}
}

func TestChannelCursor_RejectsLegacyFormat(t *testing.T) {
	// v0.8.4-shape cursor (raw msg_id) must NOT parse.
	_, _, _, err := DecodeChannelCursor("msg_18aec3225c5a78b03dd59623")
	if err == nil {
		t.Error("legacy msg_<hex> cursor should be rejected")
	}
}

func TestChannelCursor_RejectsTruncatedToken(t *testing.T) {
	_, _, _, err := DecodeChannelCursor("cur_short")
	if err == nil {
		t.Error("truncated cursor should be rejected")
	}
}

// PR 1 review fix: trailing junk in the msg_id portion previously
// passed silently → stuck subscriber. Must now refuse.
func TestChannelCursor_RejectsMsgIDWithTrailingJunk(t *testing.T) {
	visAt := time.Unix(0, 1747042420123456789).UTC()
	msgID := MintChannelMessageID(visAt)
	// Append garbage to the msg_id portion.
	bad := EncodeChannelCursor(visAt, msgID) + "_garbage"
	_, _, _, err := DecodeChannelCursor(bad)
	if err == nil {
		t.Error("cursor with trailing junk in msg_id should be rejected")
	}
	if !strings.Contains(err.Error(), "msg_id") {
		t.Errorf("error should mention msg_id; got %v", err)
	}
}

// PR 1 review fix: msg_id with wrong byte length is rejected.
func TestChannelCursor_RejectsMsgIDWrongLength(t *testing.T) {
	// msg_id should be `msg_<24hex>`. This one has only 8 hex chars.
	bad := "cur_0001a2b3c4d5e6f7_msg_abcd1234"
	_, _, _, err := DecodeChannelCursor(bad)
	if err == nil {
		t.Error("short msg_id should be rejected")
	}
}

// PR 1 review fix: msg_id with non-hex chars rejected.
func TestChannelCursor_RejectsMsgIDNonHex(t *testing.T) {
	// 24 chars but with a Z in there.
	bad := "cur_0001a2b3c4d5e6f7_msg_abcdef0123456789Zbcdef01"
	_, _, _, err := DecodeChannelCursor(bad)
	if err == nil {
		t.Error("non-hex char in msg_id should be rejected")
	}
}
