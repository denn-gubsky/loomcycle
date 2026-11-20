//go:build !unix

package fsops

// nonBlock is unused off unix, where there are no FIFOs to block on.
const nonBlock = 0
