package erasure

import "errors"

// ErrLocked refuses an erasure that would take the subject out of an
// access-control list the caller is denied retiring (a deny on retire
// def:acl/<name>): the transport refuses it before anything is read or
// deleted, so the lock that keeps a binding or a group membership in place
// holds against erasure too. Admin erasure is not held.
var ErrLocked = errors.New("the erasure would remove an access-control list the caller may not retire")
