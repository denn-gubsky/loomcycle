package http

import (
	"context"
	"sync"
	"sync/atomic"
)

// engineQueue bounds the engine requests one audio route has in flight,
// server-wide. A self-hosted speech engine serves a few requests at a time
// and answers the rest slowly or not at all, so the runtime holds the excess
// back itself. A request over the bound WAITS for a slot rather than being
// refused (decision P-D5 under G30): a read-aloud that starts a few seconds
// late is better than one a client has to retry. A slot is taken per engine
// request, not per HTTP request, so a long text or recording cut into pieces
// takes turns with shorter ones instead of holding the engine throughout.
type engineQueue struct {
	slots   chan struct{}
	waiting atomic.Int64
}

func newEngineQueue(n int) *engineQueue {
	if n <= 0 {
		n = 1
	}
	return &engineQueue{slots: make(chan struct{}, n)}
}

// take waits for a slot until ctx ends. The returned release gives the slot
// back and is safe to call more than once; a caller that hangs up leaves the
// queue with ctx's error and holds nothing.
func (q *engineQueue) take(ctx context.Context) (func(), error) {
	select {
	case q.slots <- struct{}{}:
		return q.releaser(), nil
	default:
	}
	q.waiting.Add(1)
	defer q.waiting.Add(-1)
	select {
	case q.slots <- struct{}{}:
		return q.releaser(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (q *engineQueue) releaser() func() {
	var once sync.Once
	return func() { once.Do(func() { <-q.slots }) }
}

// inflight and queued are the queue's state for the metrics endpoint.
func (q *engineQueue) inflight() int { return len(q.slots) }
func (q *engineQueue) queued() int64 { return q.waiting.Load() }

// audioQueues are the two routes' queues, sized from the environment the
// first time either is needed: a change of LOOMCYCLE_SPEECH_MAX_INFLIGHT or
// LOOMCYCLE_TRANSCRIPTION_MAX_INFLIGHT takes a restart.
type audioQueues struct {
	once          sync.Once
	speech, trans *engineQueue
}

func (s *Server) audioQueues() (speech, transcription *engineQueue) {
	s.audioQ.once.Do(func() {
		env := s.cfg().Env
		s.audioQ.speech = newEngineQueue(env.SpeechMaxInflight)
		s.audioQ.trans = newEngineQueue(env.TranscriptionMaxInflight)
	})
	return s.audioQ.speech, s.audioQ.trans
}
