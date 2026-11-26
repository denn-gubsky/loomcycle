package egress

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// gitpush.go — the push policy (RFC GW §6.2, with the owner's rulings of
// 2026-11-26 in the P11 plan §7.1). A push is the one git request that changes
// something outside, so the core reads the ref-update commands at the head of
// a receive-pack request, which come before the pack, and refuses the request
// unless every command passes. Nothing reaches the forge until they have.
//
// What the core cannot see is whether an update is a fast-forward: that is in
// the pack and on the forge. A forced update of the workspace's own branch is
// accepted (D11 as amended); other branches are held by the ref rule here and
// by the forge's own branch protection.

const (
	// maxPushSection bounds the command section the core reads before it
	// decides (spike B decided after 32 KiB of a 20 MB body).
	maxPushSection = 32 << 10
	// maxPushCommands bounds the commands of one push: a push of the own
	// branch has one, a tag with it two.
	maxPushCommands = 8
	// maxPktLen is git's largest pkt-line, its four length digits included.
	maxPktLen = 65520
)

// zeroOID is the object id a deletion's new value (and a creation's old) is.
const zeroOID = "0000000000000000000000000000000000000000"

// pushCapabilities are the capabilities git and go-git send on a push that
// the core passes, besides agent=…. Anything else, push-options above all,
// refuses the push: the core reads only what it knows.
var pushCapabilities = []string{"report-status", "report-status-v2", "side-band-64k", "side-band", "quiet", "atomic", "ofs-delta", "object-format=sha1"}

// PushPolicy is what one workspace's push may do.
type PushPolicy struct {
	// Branch is the workspace's own branch, by its short name.
	Branch string
	// DefaultBranch is the forge's default branch as a full ref, as the
	// core read it from the forge's own advertisement; "" while unknown,
	// when main and master both count as default.
	DefaultBranch string
	// AllowTags lets a push create a tag (workspace.push.ref_prefixes lists
	// refs/tags/). An existing tag is never moved.
	AllowTags bool
}

// PushCommand is one ref update.
type PushCommand struct{ Old, New, Ref string }

// PushSection is a receive-pack request's command section.
type PushSection struct {
	Commands     []PushCommand
	Capabilities []string
	// Raw is the section's bytes as read, the flush included: the core
	// sends them to the forge ahead of the rest of the body, unchanged.
	Raw []byte
}

func pushRefusal(format string, a ...any) *Refusal {
	return &Refusal{Status: http.StatusForbidden, Reason: "the push is refused: " + fmt.Sprintf(format, a...)}
}

// ReadPushSection reads the command section at the start of r: pkt-lines up
// to the first flush, at most maxPushSection bytes and maxPushCommands
// commands. It reads nothing past the flush. Anything else is a *Refusal:
// a malformed or oversized line, a shallow line, a capability list on any
// line but the first, a ref git would refuse.
func ReadPushSection(r io.Reader) (PushSection, error) {
	var s PushSection
	var raw bytes.Buffer
	for {
		var head [4]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return PushSection{}, pushRefusal("the request ends before its command section does")
		}
		raw.Write(head[:])
		if !isLowerHex(string(head[:])) {
			return PushSection{}, pushRefusal("a pkt-line length that is not four lower-case hex digits")
		}
		n, _ := strconv.ParseUint(string(head[:]), 16, 16)
		if n == 0 {
			break
		}
		if n < 5 || n > maxPktLen {
			return PushSection{}, pushRefusal("a pkt-line of length %d", n)
		}
		if raw.Len()+int(n)-4 > maxPushSection {
			return PushSection{}, pushRefusal("the command section is over %d KiB", maxPushSection>>10)
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(r, line); err != nil {
			return PushSection{}, pushRefusal("a pkt-line shorter than its length")
		}
		raw.Write(line)
		c, capList, err := parsePushLine(string(line), len(s.Commands) == 0)
		if err != nil {
			return PushSection{}, err
		}
		if len(s.Commands) == 0 {
			s.Capabilities = capList
		}
		s.Commands = append(s.Commands, c)
		if len(s.Commands) > maxPushCommands {
			return PushSection{}, pushRefusal("more than %d ref updates", maxPushCommands)
		}
	}
	if len(s.Commands) == 0 {
		return PushSection{}, pushRefusal("no ref update")
	}
	s.Raw = raw.Bytes()
	return s, nil
}

// parsePushLine reads "<old> <new> <ref>", with "\x00<capabilities>" on the
// first line only, and an optional newline.
func parsePushLine(line string, first bool) (PushCommand, []string, error) {
	line = strings.TrimSuffix(line, "\n")
	if strings.HasPrefix(line, "shallow ") {
		return PushCommand{}, nil, pushRefusal("a shallow push")
	}
	var capList []string
	if body, capText, ok := strings.Cut(line, "\x00"); ok {
		if !first {
			return PushCommand{}, nil, pushRefusal("capabilities after the first ref update")
		}
		line = body
		capList = strings.Fields(capText)
	}
	parts := strings.Split(line, " ")
	if len(parts) != 3 || !isOID(parts[0]) || !isOID(parts[1]) {
		return PushCommand{}, nil, pushRefusal("a ref update that is not \"<old> <new> <ref>\" with 40-digit lower-case ids")
	}
	if err := checkRefName(parts[2]); err != nil {
		return PushCommand{}, nil, err
	}
	return PushCommand{Old: parts[0], New: parts[1], Ref: parts[2]}, capList, nil
}

func isOID(s string) bool { return len(s) == 40 && isLowerHex(s) }

func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// checkRefName holds a ref name to git's check-ref-format rules, and to
// refs/: no "..", "@{", "//", control character, space, ~^:?*[\, a part
// starting with "." or ending in ".lock", or a trailing "/" or ".".
func checkRefName(ref string) error {
	bad := func(why string) error {
		return pushRefusal("the ref %q %s", truncateName(ref), why)
	}
	if !strings.HasPrefix(ref, "refs/") {
		return bad("is not under refs/")
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") {
		return bad(`holds "..", "@{" or "//"`)
	}
	if strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") {
		return bad(`ends in "/" or "."`)
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if c < 0x20 || c == 0x7f || strings.IndexByte(" ~^:?*[\\", c) >= 0 {
			return bad("holds a character git refuses")
		}
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return bad("has a part git refuses")
		}
	}
	return nil
}

// truncateName keeps a name a refusal quotes short; the name is the worker's.
func truncateName(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// Check refuses the push unless every command passes: one refused command
// refuses all of them.
func (p PushPolicy) Check(s PushSection) error {
	for _, c := range s.Capabilities {
		if strings.HasPrefix(c, "agent=") || slices.Contains(pushCapabilities, c) {
			continue
		}
		return pushRefusal("the capability %q is not one the core passes (push options and every capability outside a short list are refused)", truncateName(c))
	}
	own := "refs/heads/" + p.Branch
	for _, c := range s.Commands {
		if err := p.checkCommand(c, own); err != nil {
			return err
		}
	}
	return nil
}

func (p PushPolicy) checkCommand(c PushCommand, own string) error {
	if c.New == zeroOID {
		return pushRefusal("%s: a deletion", c.Ref)
	}
	if p.isDefault(c.Ref) {
		return pushRefusal("%s is the forge's default branch", c.Ref)
	}
	if strings.HasPrefix(c.Ref, "refs/tags/") {
		if !p.AllowTags {
			return pushRefusal("%s: a tag (the operator allows none)", c.Ref)
		}
		if c.Old != zeroOID {
			return pushRefusal("%s moves an existing tag", c.Ref)
		}
		return nil
	}
	if p.Branch == "" || c.Ref != own {
		return pushRefusal("%s is not this workspace's own branch (%s)", c.Ref, own)
	}
	return nil
}

func (p PushPolicy) isDefault(ref string) bool {
	if p.DefaultBranch != "" {
		return ref == p.DefaultBranch
	}
	return ref == "refs/heads/main" || ref == "refs/heads/master"
}
