package builtin

import "encoding/json"

// RFC FY Part A: a member reaches another subject's Path tree and documents
// off a run only through a rule that lets it browse that subject, and a
// browse is a read. The off-run routes ask these before such a call, so a
// browse grant never becomes write or delete access.

// DocumentCallReadsOnly reports whether a Document call reads and changes
// nothing. It is docACLOps's own answer — an op whose question is `read` —
// less the read-asked ops that also write: copy_documents and move_documents
// (they write each target and move removes the source), judge_statements (it
// rules), classify unless write is "report", and resolve_anchor when it files a
// placeholder. An op docACLOps does not know, or input that is not an object,
// is not a read.
func DocumentCallReadsOnly(input json.RawMessage) bool {
	var in struct {
		Op                string `json:"op"`
		Write             string `json:"write"`
		CreatePlaceholder bool   `json:"create_placeholder"`
	}
	if json.Unmarshal(input, &in) != nil {
		return false
	}
	rule, ok := docACLOps[in.Op]
	if !ok || rule.verb != "read" {
		return false
	}
	switch in.Op {
	case "copy_documents", "move_documents", "judge_statements":
		return false
	case "classify":
		return in.Write == "report"
	case "resolve_anchor":
		return !in.CreatePlaceholder
	}
	return true
}

// PathCallReadsOnly reports whether a Path call reads and changes nothing:
// resolve, ls and stat, the ops that ask `read` (pathtool.go); mkdir, mv and rm
// ask `write`.
func PathCallReadsOnly(input json.RawMessage) bool {
	var in struct {
		Op string `json:"op"`
	}
	if json.Unmarshal(input, &in) != nil {
		return false
	}
	switch in.Op {
	case "resolve", "ls", "stat":
		return true
	}
	return false
}
