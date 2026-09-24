package loop

// IdlePauser is implemented by a PauseGate that can record a WAITING run as
// paused without moving it.
//
// A run parked for its next message, or held for a review verdict, is already
// at a clean boundary: nothing is in flight. It never reaches the loop's
// pause gate, though, because it is blocked in its park — so before this it
// held a runtime pause until the pause's timeout, was reported as not
// quiesced, and stayed pause_state 'running'. That made it the one run a
// snapshot or a restart could not bring back, which is exactly the run a
// person is waiting on.
type IdlePauser interface {
	// PauseCh is closed when a pause is declared. A fresh channel is made on
	// each resume, so it is re-fetched after every cycle.
	PauseCh() <-chan struct{}
	// PauseIdle records the run as paused and credits it to the pause
	// barrier. resumed is closed when the runtime resumes; release undoes the
	// record, and is safe to call more than once. ok=false means the pause was
	// already lifted and nothing was recorded.
	PauseIdle() (resumed <-chan struct{}, release func(), ok bool)
}

// parkPause tracks one park's part in a runtime pause. The zero value (no
// IdlePauser) never fires: every channel it returns is nil.
type parkPause struct {
	p        IdlePauser
	pauseCh  <-chan struct{}
	resumeCh <-chan struct{}
	release  func()
}

func newParkPause(g PauseGate) *parkPause {
	pp := &parkPause{}
	if ip, ok := g.(IdlePauser); ok {
		pp.p = ip
		pp.pauseCh = ip.PauseCh()
	}
	return pp
}

// recording reports whether the run is currently recorded as paused.
func (pp *parkPause) recording() bool { return pp.release != nil }

// declared is the channel to select on for "a pause was declared".
func (pp *parkPause) declared() <-chan struct{} { return pp.pauseCh }

// lifted is the channel to select on for "the pause was lifted".
func (pp *parkPause) lifted() <-chan struct{} { return pp.resumeCh }

// onDeclared records the waiting run as paused.
func (pp *parkPause) onDeclared() {
	resumed, release, ok := pp.p.PauseIdle()
	if !ok {
		// Lost a race with a resume: nothing recorded, wait for the next pause.
		pp.pauseCh = pp.p.PauseCh()
		return
	}
	pp.pauseCh, pp.resumeCh, pp.release = nil, resumed, release
}

// onLifted undoes the record and waits for the next pause.
func (pp *parkPause) onLifted() {
	pp.done()
	pp.pauseCh = pp.p.PauseCh()
}

// done undoes the record if one is held. Called when the run leaves its park:
// a run that moves on while the runtime is still paused stops at the loop's
// own pause gate before doing any work, which records it again.
func (pp *parkPause) done() {
	if pp.release != nil {
		pp.release()
		pp.release = nil
	}
	pp.resumeCh = nil
}
