package audit

import (
	"errors"
	"time"
)

// multiSink records to every sink, and fails if any of them failed.
type multiSink []Sink

// Combine returns one Sink recording to each non-nil sink given, or nil when
// there is none. A caller that treats a nil sink as "nothing durable is
// written" keeps that meaning.
func Combine(sinks ...Sink) Sink {
	var out multiSink
	for _, s := range sinks {
		if s != nil {
			out = append(out, s)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	return out
}

func (m multiSink) Record(ev Event) error {
	if ev.TS.IsZero() {
		// One time for every copy, so the mirror and the chain agree.
		ev.TS = time.Now().UTC()
	}
	var errs []error
	for _, s := range m {
		errs = append(errs, s.Record(ev))
	}
	return errors.Join(errs...)
}
