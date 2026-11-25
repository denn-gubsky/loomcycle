package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// lsofPath is macOS's own lsof, never one found on PATH.
const lsofPath = "/usr/sbin/lsof"

// listenerPIDs lists the processes holding a TCP socket listening on port,
// on any address, from lsof.
func listenerPIDs(ctx context.Context, port int) ([]int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// #nosec G204 -- a fixed program; the only argument from the caller is a port number it formats
	out, err := exec.CommandContext(ctx, lsofPath, "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fp").Output()
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0) {
		// lsof exits 1 when nothing matches; anything else is a fault.
		return nil, err
	}
	var pids []int
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "p"); ok {
			pid, err := strconv.Atoi(rest)
			if err != nil {
				return nil, errors.New("lsof printed a pid that is not a number")
			}
			pids = append(pids, pid)
		}
	}
	return pids, sc.Err()
}
