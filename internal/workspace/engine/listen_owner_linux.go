package engine

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// listenerPIDs lists the processes holding a TCP socket listening on port,
// on any address: the sockets' inodes from /proc/net/tcp and tcp6, then
// every process whose descriptors name one of them.
func listenerPIDs(ctx context.Context, port int) ([]int, error) {
	inodes := map[string]bool{}
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if err := listenInodes(table, port, inodes); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	if len(inodes) == 0 {
		return nil, nil
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	found := map[string]bool{}
	for _, p := range procs {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", p.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			// Another user's process, or one gone: a listener there is
			// counted below as held by no process of ours.
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if ino, ok := strings.CutPrefix(link, "socket:["); ok && inodes[strings.TrimSuffix(ino, "]")] {
				found[strings.TrimSuffix(ino, "]")] = true
				pids = append(pids, pid)
				break
			}
		}
	}
	if len(found) < len(inodes) {
		// A listener held by a process whose descriptors cannot be read:
		// an owner that is no process of ours, so the port is refused.
		pids = append(pids, -1)
	}
	return pids, nil
}

// listenInodes adds the inodes of table's sockets listening on port.
func listenInodes(table string, port int, into map[string]bool) error {
	// #nosec G304 -- one of two fixed /proc paths
	f, err := os.Open(table)
	if err != nil {
		return err
	}
	defer f.Close()
	want := strings.ToUpper(strconv.FormatInt(int64(port), 16))
	for len(want) < 4 {
		want = "0" + want
	}
	sc := bufio.NewScanner(f)
	sc.Scan() // the header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[3] != "0A" { // 0A: LISTEN
			continue
		}
		if _, p, ok := strings.Cut(fields[1], ":"); ok && p == want {
			into[fields[9]] = true
		}
	}
	return sc.Err()
}
