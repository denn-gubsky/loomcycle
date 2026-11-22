package kernel

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/safego"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

// Bounds of what a driver may send and what a result keeps.
const (
	// maxLine bounds one frame. A longer line is read through and dropped,
	// so a cell writing one endless line holds no more than this.
	maxLine = 1 << 20
	// MaxResult bounds a cell's output, value and error together, as exec's
	// output is bounded (RFC GW §5).
	MaxResult = 1 << 20
	// MaxValue and MaxError bound the value and the error within it; the
	// output gets what they leave.
	MaxValue = 64 << 10
	MaxError = 64 << 10
	// MaxPictures bounds the pictures a result names.
	MaxPictures = 32
	// protocol is the frame protocol the drivers speak.
	protocol = 1
)

// ErrDied is a kernel that ended while a cell ran in it.
var ErrDied = errors.New("the kernel exited")

// pictureName is the only form of picture a result names: the drivers save
// figures as .loom/cells/cell-<n>-<i>.png. A frame naming anything else is
// a forgery or a bug, and its names are dropped.
var pictureName = regexp.MustCompile(`^\.loom/cells/cell-[0-9]{1,9}-[0-9]{1,4}\.png$`)

// frame is one line from a driver. Every field is untrusted.
type frame struct {
	Type        string   `json:"type"`
	ID          string   `json:"id"`
	Protocol    int      `json:"protocol"`
	Text        string   `json:"text"`
	Value       *string  `json:"value"`
	Error       string   `json:"error"`
	Interrupted bool     `json:"interrupted"`
	Pictures    []string `json:"pictures"`
	Dropped     int64    `json:"dropped"`
}

// cellState is the cell a kernel runs now: the output its frames brought,
// kept up to max, and its done frame once one came.
type cellState struct {
	id      string
	max     int
	out     strings.Builder
	dropped int64
	done    *frame
	finish  chan struct{}
}

// proc is one running driver.
type proc struct {
	p     engine.Process
	ready chan struct{}
	// exited is closed when the driver's stdout ends: it died or was killed.
	exited chan struct{}
	// dropped counts the frames the reader threw away: over maxLine, not
	// JSON, or not of the current cell. Kept for tests and logs.
	dropped int64

	mu  sync.Mutex
	cur *cellState
	// writeMu serialises frames to the driver's stdin.
	writeMu sync.Mutex
}

// start starts a driver and waits for its ready frame.
func start(ctx context.Context, eng engine.Engine, req engine.StartRequest, wait time.Duration) (*proc, error) {
	p, err := eng.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	k := &proc{p: p, ready: make(chan struct{}), exited: make(chan struct{})}
	safego.Go(context.WithoutCancel(ctx), "workspace.kernel.read", k.read)
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-k.ready:
		return k, nil
	case <-k.exited:
		<-p.Done()
		return nil, fmt.Errorf("the kernel exited before it was ready: %s", strings.TrimSpace(tail(p.Stderr(), 1000)))
	case <-t.C:
		k.kill()
		return nil, fmt.Errorf("the kernel did not start within %s", wait)
	case <-ctx.Done():
		k.kill()
		return nil, ctx.Err()
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[len(s)-n:], "")
}

// read reads frames until the driver's stdout ends. It holds one line at a
// time, at most maxLine, and keeps nothing a frame says except into the
// current cell, within the cell's bound.
func (k *proc) read() {
	defer close(k.exited)
	r := bufio.NewReaderSize(k.p.Stdout(), maxLine)
	readyOnce := sync.Once{}
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			k.dropped++
			// Read the rest of the long line through, holding none of it.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			if err != nil {
				return
			}
			continue
		}
		if err != nil {
			return
		}
		var f frame
		if json.Unmarshal(line, &f) != nil {
			k.dropped++
			continue
		}
		if f.Type == "ready" && f.Protocol == protocol {
			readyOnce.Do(func() { close(k.ready) })
			continue
		}
		k.take(&f)
	}
}

// take gives a frame to the current cell, if it is that cell's.
func (k *proc) take(f *frame) {
	k.mu.Lock()
	defer k.mu.Unlock()
	c := k.cur
	if c == nil || c.done != nil || f.ID != c.id {
		k.dropped++
		return
	}
	switch f.Type {
	case "out":
		room := c.max - c.out.Len()
		if room <= 0 {
			c.dropped += int64(len(f.Text))
			return
		}
		text := f.Text
		if len(text) > room {
			c.dropped += int64(len(text) - room)
			text = cutUTF8(text, room)
		}
		c.out.WriteString(text)
	case "done":
		c.done = f
		close(c.finish)
	default:
		k.dropped++
	}
}

func (k *proc) kill() {
	_ = k.p.Kill()
	_ = k.p.Stdin().Close()
}

func (k *proc) alive() bool {
	select {
	case <-k.exited:
		return false
	default:
		return true
	}
}

// cellRequest is one cell for a running driver.
type cellRequest struct {
	code      string
	n         int
	timeout   time.Duration
	grace     time.Duration
	maxOutput int
}

// outcome is how a cell ended on the supervisor's side.
type outcome struct {
	done     *frame
	output   string
	dropped  int64
	timedOut bool
	killed   bool
	died     bool
}

// run sends one cell and waits for its done frame, interrupting it at the
// timeout and killing the driver when the interrupt is not answered within
// the grace. When ctx ends first, run returns ctx's error at once and
// settle, run in the background by the caller, finishes the cell the same
// way; the caller's lock must be held until settle returns.
func (k *proc) run(ctx context.Context, req cellRequest) (outcome, func() outcome, error) {
	id, err := newID()
	if err != nil {
		return outcome{}, nil, err
	}
	c := &cellState{id: id, max: req.maxOutput, finish: make(chan struct{})}
	k.mu.Lock()
	k.cur = c
	k.mu.Unlock()
	line, err := json.Marshal(map[string]any{
		"type": "cell", "id": id, "code": req.code, "n": req.n,
		"max_output": req.maxOutput, "max_value": MaxValue,
	})
	if err != nil {
		return outcome{}, nil, err
	}
	// The write may block while the driver is busy with a cell that forged
	// its own end; it is waited for as part of the cell.
	written := make(chan error, 1)
	safego.Go(context.WithoutCancel(ctx), "workspace.kernel.write", func() {
		k.writeMu.Lock()
		defer k.writeMu.Unlock()
		_, err := k.p.Stdin().Write(append(line, '\n'))
		written <- err
	})
	timer := time.NewTimer(req.timeout)
	defer timer.Stop()
	select {
	case <-c.finish:
		return k.collect(c, outcome{}), nil, nil
	case <-k.exited:
		return k.collect(c, outcome{died: true}), nil, nil
	case err := <-written:
		if err != nil {
			k.kill()
			return k.collect(c, outcome{died: true}), nil, nil
		}
	case <-timer.C:
		return k.stop(c, req.grace), nil, nil
	case <-ctx.Done():
		return outcome{}, func() outcome { return k.stop(c, req.grace) }, ctx.Err()
	}
	// Written; now wait for the end.
	select {
	case <-c.finish:
		return k.collect(c, outcome{}), nil, nil
	case <-k.exited:
		return k.collect(c, outcome{died: true}), nil, nil
	case <-timer.C:
		return k.stop(c, req.grace), nil, nil
	case <-ctx.Done():
		return outcome{}, func() outcome { return k.stop(c, req.grace) }, ctx.Err()
	}
}

// stop interrupts the cell, waits the grace for its end, and kills the
// driver when none comes.
func (k *proc) stop(c *cellState, grace time.Duration) outcome {
	_ = k.p.Interrupt()
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-c.finish:
		return k.collect(c, outcome{timedOut: true})
	case <-k.exited:
		return k.collect(c, outcome{timedOut: true, died: true})
	case <-t.C:
	}
	k.kill()
	select {
	case <-k.exited:
	case <-time.After(5 * time.Second):
	}
	return k.collect(c, outcome{timedOut: true, killed: true})
}

// collect takes what the cell's frames brought and ends the cell: a frame
// that comes after has no cell to go to.
func (k *proc) collect(c *cellState, o outcome) outcome {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cur == c {
		k.cur = nil
	}
	o.done = c.done
	o.output = c.out.String()
	o.dropped = c.dropped
	return o
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// cutUTF8 is s cut to at most n bytes, on a rune boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
