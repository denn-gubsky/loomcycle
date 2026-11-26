package egress

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// pkt is one git pkt-line holding s.
func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

const (
	zeroID = "0000000000000000000000000000000000000000"
	oldID  = "1111111111111111111111111111111111111111"
	newID  = "2222222222222222222222222222222222222222"
	caps   = "report-status side-band-64k agent=go-git/5.19.3"
)

// section is a receive-pack command section: the first command carries the
// capabilities, then a flush; pack bytes follow it.
func section(cmds ...string) string {
	var b strings.Builder
	for i, c := range cmds {
		if i == 0 {
			c += "\x00" + caps
		}
		b.WriteString(pkt(c + "\n"))
	}
	b.WriteString("0000")
	return b.String()
}

func cmd(old, new, ref string) string { return old + " " + new + " " + ref }

var ownPolicy = PushPolicy{Branch: "loom/fix-1", DefaultBranch: "refs/heads/main"}

func readSection(t *testing.T, body string) (PushSection, error) {
	t.Helper()
	return ReadPushSection(strings.NewReader(body + "PACK…"))
}

func wantPushRefusal(t *testing.T, err error, says string) {
	t.Helper()
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Status != http.StatusForbidden {
		t.Fatalf("err = %v, want a 403 refusal", err)
	}
	if !strings.Contains(ref.Reason, says) {
		t.Fatalf("reason %q does not say %q", ref.Reason, says)
	}
}

// The policy is RFC GW §6.2 with the owner's rulings of 2026-11-26 (P11 plan
// §7.1): a push moves the workspace's own branch only, never the default
// branch, never deletes, and writes a tag only where the operator allows
// tags, and then only a new one.
func TestPushPolicy_RefusesEveryRefButTheOwnBranch(t *testing.T) {
	cases := []struct {
		name   string
		policy PushPolicy
		cmds   []string
		says   string // "" = accepted
	}{
		{"own branch update", ownPolicy, []string{cmd(oldID, newID, "refs/heads/loom/fix-1")}, ""},
		{"own branch created", ownPolicy, []string{cmd(zeroID, newID, "refs/heads/loom/fix-1")}, ""},
		{"another branch", ownPolicy, []string{cmd(oldID, newID, "refs/heads/feature")}, "not this workspace's own branch"},
		{"the default branch", ownPolicy, []string{cmd(oldID, newID, "refs/heads/main")}, "default branch"},
		{"own branch is the default", PushPolicy{Branch: "main", DefaultBranch: "refs/heads/main"}, []string{cmd(oldID, newID, "refs/heads/main")}, "default branch"},
		{"default unknown: main", PushPolicy{Branch: "main"}, []string{cmd(oldID, newID, "refs/heads/main")}, "default branch"},
		{"default unknown: master", PushPolicy{Branch: "master"}, []string{cmd(oldID, newID, "refs/heads/master")}, "default branch"},
		{"a deletion of the own branch", ownPolicy, []string{cmd(oldID, zeroID, "refs/heads/loom/fix-1")}, "deletion"},
		{"a tag", ownPolicy, []string{cmd(zeroID, newID, "refs/tags/v1")}, "tag"},
		{"a new tag where tags are allowed", PushPolicy{Branch: "loom/fix-1", DefaultBranch: "refs/heads/main", AllowTags: true}, []string{cmd(zeroID, newID, "refs/tags/v1")}, ""},
		{"a tag moved where tags are allowed", PushPolicy{Branch: "loom/fix-1", DefaultBranch: "refs/heads/main", AllowTags: true}, []string{cmd(oldID, newID, "refs/tags/v1")}, "moves an existing tag"},
		{"a note", ownPolicy, []string{cmd(oldID, newID, "refs/notes/commits")}, "not this workspace's own branch"},
		{"mixed: own and another", ownPolicy, []string{cmd(oldID, newID, "refs/heads/loom/fix-1"), cmd(oldID, newID, "refs/heads/main")}, "default branch"},
		{"a prefix of the own branch", ownPolicy, []string{cmd(oldID, newID, "refs/heads/loom/fix-10")}, "not this workspace's own branch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := readSection(t, section(c.cmds...))
			if err != nil {
				t.Fatalf("ReadPushSection: %v", err)
			}
			err = c.policy.Check(s)
			if c.says == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			wantPushRefusal(t, err, c.says)
		})
	}
}

// Push options, and any capability outside the short list git and go-git
// send, are refused: the core reads only what it knows.
func TestPushPolicy_RefusesPushOptionsAndUnknownCapabilities(t *testing.T) {
	for name, capList := range map[string]string{
		"push options": "report-status push-options",
		"unknown":      "report-status no-such-capability",
		"a delete cap": "report-status delete-refs",
		"other format": "report-status object-format=sha256",
		"a push cert":  "report-status push-cert=1234",
	} {
		t.Run(name, func(t *testing.T) {
			body := pkt(cmd(oldID, newID, "refs/heads/loom/fix-1")+"\x00"+capList+"\n") + "0000"
			s, err := readSection(t, body)
			if err == nil {
				err = ownPolicy.Check(s)
			}
			wantPushRefusal(t, err, "capabilit")
		})
	}
	ok := pkt(cmd(oldID, newID, "refs/heads/loom/fix-1")+"\x00report-status report-status-v2 side-band-64k side-band quiet atomic ofs-delta object-format=sha1 agent=git/2.47.0\n") + "0000"
	s, err := readSection(t, ok)
	if err != nil {
		t.Fatal(err)
	}
	if err := ownPolicy.Check(s); err != nil {
		t.Fatalf("the capabilities git sends were refused: %v", err)
	}
}

// What the reader refuses before any policy: anything not a well-formed,
// bounded command section of the protocol the core speaks (v0).
func TestReadPushSection_RefusesWhatIsNotAWellFormedSection(t *testing.T) {
	own := cmd(oldID, newID, "refs/heads/loom/fix-1")
	many := make([]string, 9)
	for i := range many {
		many[i] = cmd(oldID, newID, fmt.Sprintf("refs/heads/b%d", i))
	}
	long := cmd(oldID, newID, "refs/heads/"+strings.Repeat("a", 40000))
	cases := map[string]string{
		"not hex length":        "zzzz" + own,
		"length 1":              "0001",
		"length 3":              "0003",
		"length over the max":   "fff1" + strings.Repeat("a", 100),
		"a shallow line":        pkt("shallow "+oldID+"\n") + section(own),
		"no flush":              pkt(own + "\x00" + caps + "\n"),
		"truncated line":        "0050" + own[:10],
		"short old id":          section("1111 " + newID + " refs/heads/loom/fix-1"),
		"upper-case id":         section(strings.Repeat("A", 40) + " " + newID + " refs/heads/loom/fix-1"),
		"ref outside refs/":     section(cmd(oldID, newID, "HEAD")),
		"ref with ..":           section(cmd(oldID, newID, "refs/heads/a..b")),
		"ref with @{":           section(cmd(oldID, newID, "refs/heads/a@{1}")),
		"ref ending .lock":      section(cmd(oldID, newID, "refs/heads/a.lock")),
		"ref with a dot part":   section(cmd(oldID, newID, "refs/heads/.hidden")),
		"ref ending /":          section(cmd(oldID, newID, "refs/heads/a/")),
		"ref with a space":      section(cmd(oldID, newID, "refs/heads/a b")),
		"ref with a control":    section(cmd(oldID, newID, "refs/heads/a\x01b")),
		"ref with a backslash":  section(cmd(oldID, newID, "refs/heads/a\\b")),
		"ref with //":           section(cmd(oldID, newID, "refs/heads//a")),
		"too many commands":     section(many...),
		"a section over 32 KiB": section(long),
		"no command":            "0000",
		"empty body":            "",
		"a NUL in a later line": section(own, cmd(oldID, newID, "refs/heads/x")+"\x00more"),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ReadPushSection(strings.NewReader(body))
			wantPushRefusal(t, err, "")
		})
	}
}

// The reader consumes exactly the section, so what it returns as Raw plus
// what is left of the body is the body, byte for byte: the core replays it
// to the forge unchanged.
func TestReadPushSection_RawPlusTheRestIsTheBody(t *testing.T) {
	body := section(cmd(oldID, newID, "refs/heads/loom/fix-1")) + "PACK\x00\x00\x00\x02rest-of-pack"
	r := strings.NewReader(body)
	s, err := ReadPushSection(r)
	if err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(r)
	if got := string(s.Raw) + string(rest); got != body {
		t.Fatalf("raw+rest = %q, want %q", got, body)
	}
	if len(s.Commands) != 1 || s.Commands[0].Ref != "refs/heads/loom/fix-1" || s.Commands[0].Old != oldID || s.Commands[0].New != newID {
		t.Fatalf("commands = %+v", s.Commands)
	}
}

// The reader never reads past the section's bound, whatever the body holds.
func TestReadPushSection_ReadsAtMostTheBound(t *testing.T) {
	cr := &countReader{r: strings.NewReader(strings.Repeat("0", 10<<20))}
	_, _ = ReadPushSection(cr)
	if cr.n > maxPushSection+4 {
		t.Fatalf("read %d bytes, the bound is %d", cr.n, maxPushSection)
	}
}

type countReader struct {
	r io.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func FuzzReadPushSection(f *testing.F) {
	f.Add([]byte(section(cmd(oldID, newID, "refs/heads/loom/fix-1")) + "PACK"))
	f.Add([]byte(pkt("shallow "+oldID+"\n") + "0000"))
	f.Add([]byte("ffff"))
	f.Add([]byte("0004"))
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		s, err := ReadPushSection(r)
		if err != nil {
			var ref *Refusal
			if !errors.As(err, &ref) {
				t.Fatalf("an error that is not a refusal: %v", err)
			}
			return
		}
		if len(s.Raw) > maxPushSection || len(s.Commands) == 0 || len(s.Commands) > maxPushCommands {
			t.Fatalf("raw %d, commands %d", len(s.Raw), len(s.Commands))
		}
		if !bytes.HasPrefix(data, s.Raw) {
			t.Fatal("raw is not the body's start")
		}
		for _, c := range s.Commands {
			if checkRefName(c.Ref) != nil {
				t.Fatalf("an invalid ref passed: %q", c.Ref)
			}
		}
		_ = ownPolicy.Check(s)
	})
}
