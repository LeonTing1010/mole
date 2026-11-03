package core

import (
	"bytes"
	"sync"
	"time"
)

// floodMarker is how Go renders syscall.ENOBUFS, which is what sing-box prints
// when its UDP socket refuses another datagram:
//
//	connection: connection download closed: write udp 192.0.2.34:50774->
//	198.51.100.20:443: write: no buffer space available
//
// That line is the one symptom that separates the two failures self-healing
// keeps confusing. Brutal sends at the configured rate whether or not the link
// can carry it, so a ceiling set above the link fills the send buffer and this
// is what comes out. A path that has simply gone dark produces no such line —
// nothing is being refused locally, it is just not arriving at the far end.
//
// The distinction is not academic. On 2026-08-22 the tunnel went dark 25 times
// (~21 minutes total). Every one of the 470 ENOBUFS lines that day fell inside
// a single event at 16:10; the other 24 blackouts produced none. Self-healing
// could not tell them apart, so it lowered the ceiling a rung for each — 24
// descents that halved the bandwidth for at least fifteen minutes apiece
// against a failure no ceiling can fix.
const floodMarker = "write: no buffer space available"

// floodSensor counts floodMarker sightings in a byte stream. It is an
// io.Writer so it can be spliced into sing-box's own output rather than
// re-reading the log file, which rotates under us.
type floodSensor struct {
	mu       sync.Mutex
	seen     int
	last     time.Time
	sequence uint64
	// tail carries the bytes that could still be the head of a marker split
	// across two writes. It is kept one byte shorter than the marker, so it can
	// never contain a complete one on its own and nothing is counted twice.
	tail []byte
}

func (f *floodSensor) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	buf := p
	if len(f.tail) > 0 {
		buf = make([]byte, 0, len(f.tail)+len(p))
		buf = append(append(buf, f.tail...), p...)
	}
	if n := bytes.Count(buf, []byte(floodMarker)); n > 0 {
		f.seen += n
		f.sequence += uint64(n)
		f.last = time.Now()
	}
	if keep := len(floodMarker) - 1; len(buf) > keep {
		f.tail = append(f.tail[:0], buf[len(buf)-keep:]...)
	} else {
		f.tail = append(f.tail[:0], buf...)
	}
	return len(p), nil
}

type floodSnapshot struct {
	sequence uint64
	last     time.Time
}

// snapshot is independent of the consuming minute-scale self-healing counter.
func (f *floodSensor) snapshot() floodSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return floodSnapshot{f.sequence, f.last}
}

// take reports the sightings since the last call and resets the count.
//
// Consuming rather than windowed on purpose: the caller ticks once a minute and
// wants "did we flood during the minute that just passed", so that a single
// short burst cannot be read as flooding by two consecutive ticks and descend a
// rung on its own. Descending then requires flooding that actually persists.
func (f *floodSensor) take() (int, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, last := f.seen, f.last
	f.seen = 0
	return n, last
}

// singboxFlood watches the live sing-box process; see Start.
var singboxFlood floodSensor

// TakeFloodSightings reports how many ENOBUFS lines sing-box has emitted since
// the last call, and resets the count.
func TakeFloodSightings() int {
	n, _ := singboxFlood.take()
	return n
}
