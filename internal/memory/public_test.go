package memory

import (
	"reflect"
	"strings"
	"testing"
)

// An unedited profile publishes nothing: the template's guidance is a comment.
func TestPublicSection_TheShippedTemplatePublishesNothing(t *testing.T) {
	if !strings.Contains(UserRootTemplate(), "\n## Public\n") {
		t.Fatal("the template has no Public section")
	}
	if got, _ := PublicSection(UserRootTemplate()); got != "" {
		t.Errorf("an unedited profile publishes %q", got)
	}
}

const editedProfile = "# User Profile\n\nIntro.\n\n## Identity\n\n- `@name` Ada Lovelace\n- `@alias` Ada\n\n" +
	"## Public\n\n<!-- guidance -->\n\nStaff engineer, payments.\n\n### Reach me\n\n#payments on chat.\n\n" +
	"```\n## not a heading\n```\n\n" +
	"## Role and context\n\nOwns the ledger.\n\n- `@name` Secret Name\n\n## Standing preferences\n\nNever push.\n"

// The Public section is returned without anything around it: not the
// private sections, not a name bullet written outside Identity.
func TestPublicSection_ReturnsThePublicSectionOnly(t *testing.T) {
	got, truncated := PublicSection(editedProfile)
	if truncated {
		t.Error("a short section reported truncated")
	}
	want := "Staff engineer, payments.\n\n### Reach me\n\n#payments on chat.\n\n```\n## not a heading\n```"
	if got != want {
		t.Errorf("public = %q, want %q", got, want)
	}
	for _, private := range []string{"ledger", "Never push", "guidance", "Secret"} {
		if strings.Contains(got, private) {
			t.Errorf("public section carries %q", private)
		}
	}
	if names := IdentityNames(editedProfile); !reflect.DeepEqual(names, []string{"Ada Lovelace", "Ada"}) {
		t.Errorf("names = %v: a bullet outside Identity must not be read", names)
	}
}

func TestPublicSection_CutsAtTheCapOnARuneBoundary(t *testing.T) {
	md := "# User Profile\n\n## Public\n\n" + strings.Repeat("é", PublicMaxBytes) + "\n"
	got, truncated := PublicSection(md)
	if !truncated || len(got) > PublicMaxBytes || !strings.HasPrefix(got, "é") || strings.ContainsRune(got, '�') {
		t.Errorf("truncated=%v len=%d", truncated, len(got))
	}
}

func TestUserRootTemplateFor_SeedsTheDisplayNameIntoThePublicSection(t *testing.T) {
	md := UserRootTemplateFor("  Ada\n## Owned\tLovelace ")
	got, _ := PublicSection(md)
	if got != "Name: Ada ## Owned Lovelace" {
		t.Errorf("seeded public = %q", got)
	}
	if strings.Count(md, "\n## ") != strings.Count(UserRootTemplate(), "\n## ") {
		t.Error("seeding added or removed a section")
	}
	if UserRootTemplateFor(" \n\t") != UserRootTemplate() {
		t.Error("an empty name changed the template")
	}
	if names := ParseSelfNames(md); len(names) != 0 {
		t.Errorf("a seeded profile declares names %v", names)
	}
}
