package cmd

import (
	"errors"
	"github.com/LeonTing1010/mole/utils"
	"strings"
	"testing"
)

func TestDialStatusDoesNotReportIncompleteOrBadSamplesAsNormal(t *testing.T) {
	// Quality/what-x-what. Why: startup and post-switch samples must be visible in both status commands.
	var st utils.State
	if !strings.Contains(dialStatus(&st), "collecting") {
		t.Fatal("empty window reported healthy")
	}
	st.RecordDial(0, errors.New("timeout"))
	if s := dialStatus(&st); !strings.Contains(s, "1/1") || !strings.Contains(s, "collecting") || strings.Contains(s, "normally") {
		t.Fatal(s)
	}
	for i := 1; i < utils.DialWindow; i++ {
		st.RecordDial(200, nil)
	}
	if s := dialStatus(&st); !strings.Contains(s, "1/20") || strings.Contains(s, "normally") {
		t.Fatal(s)
	}
	st.RecordDial(200, nil)
	if !strings.Contains(dialStatus(&st), "normally") {
		t.Fatal("full clean window not reported normal")
	}
}

func TestDialStatusSeparatesSlowRepliesFromFailures(t *testing.T) {
	var st utils.State
	st.RecordDial(utils.DialSlowMs+1, nil)
	st.RecordDial(utils.DialSlowMs, nil)
	st.RecordDial(0, errors.New("timeout"))
	got := dialStatus(&st)
	if !strings.Contains(got, "2/3") || !strings.Contains(got, "1 slow >1000ms, 1 failed") {
		t.Fatal(got)
	}
}
