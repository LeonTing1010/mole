package core

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// The line the sensor has to recognise, verbatim from ~/.mole/mole.log.
const enobufsLine = "ERROR[17673] [3127740104 46.49s] connection: connection download closed: " +
	"write udp 192.0.2.34:50774->198.51.100.20:443: write: no buffer space available\n"

func TestFloodSensorCountsEnobufsLines(t *testing.T) {
	var f floodSensor
	io.WriteString(&f, "INFO inbound/tun: started at utun4\n")
	io.WriteString(&f, enobufsLine)
	io.WriteString(&f, enobufsLine)
	if n, _ := f.take(); n != 2 {
		t.Errorf("sightings = %d, want 2", n)
	}
}

// sing-box's output arrives in whatever chunks the pipe hands over, so a marker
// can straddle two writes. Missing those would under-report exactly during the
// bursts that matter, when the log is busiest.
func TestFloodSensorCountsAMarkerSplitAcrossWrites(t *testing.T) {
	var f floodSensor
	for i := 1; i < len(enobufsLine); i++ {
		f = floodSensor{}
		io.WriteString(&f, enobufsLine[:i])
		io.WriteString(&f, enobufsLine[i:])
		if n, _ := f.take(); n != 1 {
			t.Fatalf("marker split at byte %d counted %d times, want 1", i, n)
		}
	}
}

// The carried tail must not let a marker be counted twice, or a quiet path
// looks like a flooding one and the ceiling descends for nothing.
func TestFloodSensorDoesNotDoubleCountAcrossTheTail(t *testing.T) {
	var f floodSensor
	io.WriteString(&f, enobufsLine)
	io.WriteString(&f, "INFO nothing to see here\n")
	io.WriteString(&f, "INFO still nothing\n")
	if n, _ := f.take(); n != 1 {
		t.Errorf("sightings = %d, want 1 — the tail re-counted a marker it had already seen", n)
	}
}

// take() is consuming: the scheduler asks once a minute and must be told about
// the minute that just passed, not about every burst since the tunnel came up.
func TestFloodSensorTakeResets(t *testing.T) {
	var f floodSensor
	io.WriteString(&f, enobufsLine)
	if n, _ := f.take(); n != 1 {
		t.Fatalf("first take = %d, want 1", n)
	}
	if n, _ := f.take(); n != 0 {
		t.Errorf("second take = %d, want 0 — take must consume", n)
	}
}

// The sensor sits in an io.MultiWriter in front of the log file. A short write
// or an error from it makes MultiWriter return ErrShortWrite, which stops
// os/exec's copier — and sing-box's log, the only durable record of a blackout,
// goes silent. Diagnostics must never be able to do that.
func TestFloodSensorNeverBreaksTheLogWriter(t *testing.T) {
	var f floodSensor
	var log bytes.Buffer
	sink := io.MultiWriter(&log, &f)

	body := strings.Repeat(enobufsLine, 3) + "INFO tail\n"
	if n, err := io.WriteString(sink, body); err != nil || n != len(body) {
		t.Fatalf("write through the multiwriter = (%d, %v), want (%d, nil)", n, err, len(body))
	}
	if log.String() != body {
		t.Error("the log file did not receive the bytes verbatim")
	}
	if n, _ := f.take(); n != 3 {
		t.Errorf("sightings = %d, want 3", n)
	}
}
