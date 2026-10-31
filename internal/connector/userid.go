package connector

// UserIDMaxLen bounds a user id. A sign-in gate's subject is
// "<gate>:<provider id>": a gate name of up to 32 characters, the colon, and
// a provider id of up to 255 (authgate/identity.MaxExternalSubject).
const UserIDMaxLen = 320

// UserIDRule names the shape ValidUserID accepts, for refusals.
const UserIDRule = "user_id must be 1-320 characters: letters, digits and _ - : . @ |"

// ValidUserID reports whether s may name a user. Beyond [A-Za-z0-9_-] it
// takes the characters sign-in gates put in their subjects: ':' (every
// "<gate>:<id>" subject), '.' and '@' (e-mail-shaped provider ids) and '|'
// (Auth0's "auth0|…"). '/' is never one: a user id is a URL path segment
// (/v1/users/{user_id}/…) and the access check splits the path on '/' to name
// the resource. Every store that turns a user id into a file or a database
// name hashes or sanitizes it first; elsewhere it is a bound SQL parameter.
func ValidUserID(s string) bool {
	if s == "" || len(s) > UserIDMaxLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == ':', r == '.', r == '@', r == '|':
			continue
		default:
			return false
		}
	}
	return true
}

// UserIDAllowed reports whether a caller whose own subject is own may send s
// as a user id: its own subject always (a gate may hand out subjects
// ValidUserID would refuse, such as an AWS role ARN with '/', and a caller
// naming itself is never a malformed request), any other value only when
// ValidUserID takes it. Not for a URL path segment: there '/' must stay out
// whoever sends it.
func UserIDAllowed(own, s string) bool {
	return (s != "" && s == own) || ValidUserID(s)
}
