package connector

import (
	"strings"
	"testing"
)

func TestValidUserID_TakesGateSubjectsAndRefusesPathAndSpace(t *testing.T) {
	for _, ok := range []string{
		"denn",
		"user_1-a",
		"pocketid:2f7d3d17-82dd-43c7-bb3c-678a8aff8860",
		"cloud:abcdefghijklmnop",
		"auth0:auth0|5f7c8ec7c33c6c004bbafe82",
		"github:12345",
		"oidc:person@example.com",
		"g:" + strings.Repeat("a", UserIDMaxLen-2),
	} {
		if !ValidUserID(ok) {
			t.Errorf("ValidUserID(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"",
		"has space",
		"a/b",
		"../etc",
		"a?b",
		"a#b",
		"a%2Fb",
		"tab\there",
		"new\nline",
		"ünicode",
		strings.Repeat("a", UserIDMaxLen+1),
	} {
		if ValidUserID(bad) {
			t.Errorf("ValidUserID(%q) = true, want false", bad)
		}
	}
}

func TestUserIDAllowed_OwnSubjectAlwaysOtherValuesByRule(t *testing.T) {
	arn := "awsiam:arn:aws:sts::123456789012:assumed-role/ci/session"
	if !UserIDAllowed(arn, arn) {
		t.Error("a caller's own subject was refused")
	}
	if UserIDAllowed("someone-else", arn) {
		t.Error("another caller's '/' subject was taken")
	}
	if UserIDAllowed("", "") {
		t.Error("an empty user id was taken")
	}
	if !UserIDAllowed("", "pocketid:x") {
		t.Error("a well-formed id was refused with no own subject")
	}
}
