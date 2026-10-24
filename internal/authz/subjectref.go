package authz

// SubjectRef names the subject a set is compiled for, as a record that
// outlives the request keeps it: a trigger definition's owner (RFC EZ §1.5),
// whose set is compiled again each time the trigger fires. Ids and a class
// only — never a bearer, and not the compiled rules, so a binding changed
// after the definition was written holds at the next fire.
type SubjectRef struct {
	TenantID   string `json:"tenant_id"`
	Subject    string `json:"subject"`
	TokenDefID string `json:"token_def_id,omitempty"`
	// Class is the token class the set's baseline is chosen for
	// (aclres.ClassKey), as Subject.Class.
	Class string `json:"class"`
}

// RefOf records s; nil when s names no subject.
func RefOf(s Subject) *SubjectRef {
	if s.ID == "" {
		return nil
	}
	return &SubjectRef{TenantID: s.TenantID, Subject: s.ID, TokenDefID: s.TokenDefID, Class: s.Class}
}

// AsSubject is the reference as the resolver reads it; Groups are recomputed
// when it is compiled.
func (r SubjectRef) AsSubject() Subject {
	return Subject{TenantID: r.TenantID, ID: r.Subject, TokenDefID: r.TokenDefID, Class: r.Class}
}
