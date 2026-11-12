package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// joinProgressEvery is the longest an open spawn_runs join goes without a
// progress notification. An MCP client aborts a call that sends "no response
// or progress" for its idle limit (30 minutes was seen), losing the envelope
// while the runs go on (RFC GK D7); a beat a minute keeps it well inside any
// such limit.
const joinProgressEvery = 60 * time.Second

// maxProgressTokenBytes bounds the token the server echoes on every
// notification. Real clients send a short string or a number.
const maxProgressTokenBytes = 256

// progressTokenOf reads params._meta.progressToken from a tools/call's params.
// Only a JSON string or number is a token (the MCP spec's two forms); anything
// else, or one too long to echo, is treated as absent.
func progressTokenOf(params json.RawMessage) json.RawMessage {
	var p struct {
		Meta struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	tok := p.Meta.ProgressToken
	if len(tok) == 0 || len(tok) > maxProgressTokenBytes {
		return nil
	}
	var v any
	if json.Unmarshal(tok, &v) != nil {
		return nil
	}
	switch v.(type) {
	case string, float64:
		return tok
	}
	return nil
}

// progressParams is a notifications/progress payload. Progress counts the
// notifications sent for the call, because the spec requires it to increase
// with each one and a heartbeat settles no child; how many children have
// settled is in Message. Total is left out for the same reason.
type progressParams struct {
	ProgressToken json.RawMessage `json:"progressToken"`
	Progress      int             `json:"progress"`
	Message       string          `json:"message"`
}

// joinProgress reports an open spawn_runs join to the client.
type joinProgress struct {
	env   *handlerEnv
	total int

	mu      sync.Mutex // orders the counter with the frame it is sent in
	sent    int
	settled int
}

func (p *joinProgress) send(settled int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settled += settled
	p.sent++
	p.env.notify("notifications/progress", progressParams{
		ProgressToken: p.env.progressToken,
		Progress:      p.sent,
		Message:       fmt.Sprintf("%d of %d runs settled", p.settled, p.total),
	})
}

// startJoinProgress arms progress notifications for a spawn_runs join when the
// client sent a progress token: one now, one each time a child settles, and
// one at least every progressEvery. A detached batch returns at once and gets
// none. The returned stop must be called before the call's result is written;
// no notification is sent after it returns.
func startJoinProgress(ctx context.Context, env *handlerEnv, req *connector.BatchSpawnRequest) (stop func()) {
	if env.progressToken == nil || env.notify == nil || req.Mode == "detach" {
		return func() {}
	}
	p := &joinProgress{env: env, total: len(req.Spawns)}
	req.OnChildSettled = func(int, connector.SpawnRunResult) { p.send(1) }
	p.send(0)

	every := env.progressEvery
	if every <= 0 {
		every = joinProgressEvery
	}
	done, exited := make(chan struct{}), make(chan struct{})
	safego.Go(ctx, "mcp.join_progress", func() {
		defer close(exited)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				p.send(0)
			}
		}
	})
	return func() {
		close(done)
		<-exited
	}
}
