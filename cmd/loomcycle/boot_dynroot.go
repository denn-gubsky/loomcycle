package main

import (
	"fmt"
	"log/slog"
	"os"
)

// checkDynamicRootWritable creates and removes a probe directory under the
// dynamic_root volume. The runtime makes run inboxes there
// (_ephemeral/r_<run>) at first use, so a root the runtime user cannot write
// otherwise surfaces only when a File download or a kept tool result fails
// with "permission denied". Boot does not fail: the operator learns of it now,
// and runs that never keep a file are unaffected.
func checkDynamicRootWritable(log *slog.Logger, root string) {
	dir, err := os.MkdirTemp(root, ".loomcycle-write-probe-")
	if err == nil {
		if err = os.Remove(dir); err == nil {
			return
		}
	}
	uid, gid := os.Geteuid(), os.Getegid()
	log.Error("volumes: the dynamic_root volume is not writable by the runtime user; every File download and kept tool result will fail until it is",
		"path", root, "uid", uid, "gid", gid,
		"remedy", fmt.Sprintf("chown -R %d:%d <the host directory mounted at %s>", uid, gid, root),
		"err", err)
}
