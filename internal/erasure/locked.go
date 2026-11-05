package erasure

import "errors"

// ErrLocked refuses an erasure that would take the subject out of an
// access-control list the caller is denied retiring (a deny on retire
// def:acl/<name>): the transport refuses it before anything is read or
// deleted, so the lock that keeps a binding or a group membership in place
// holds against erasure too. Admin erasure is not held.
var ErrLocked = errors.New("the erasure would remove an access-control list the caller may not retire")

// ErrACLUnavailable refuses an erasure whose lock check could not be
// evaluated (the caller's access-control set or the subject's lists could
// not be read): nothing was read or deleted, and retrying is the way out.
// It is not ErrLocked, which says a lock holds.
var ErrACLUnavailable = errors.New("access control could not be evaluated for the erasure")
