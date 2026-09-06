package udp

import (
	"math"
	"testing"
)

func TestReplayWindowRejectsDuplicatesAndOldPackets(t *testing.T) {
	var window ReplayWindow
	if window.Seen(100) {
		t.Fatal("new packet unexpectedly seen")
	}
	if !window.MarkCompleted(100) || !window.Seen(100) {
		t.Fatal("completed packet was not recorded")
	}
	if window.MarkCompleted(100) {
		t.Fatal("duplicate packet was accepted")
	}
	if !window.MarkCompleted(102) || !window.MarkCompleted(101) {
		t.Fatal("in-window reordering should be accepted")
	}
	if window.MarkCompleted(20) {
		t.Fatal("packet older than replay window was accepted")
	}
}

func TestReplayWindowHandlesPacketIDWraparound(t *testing.T) {
	var window ReplayWindow
	if !window.MarkCompleted(math.MaxUint32-1) || !window.MarkCompleted(math.MaxUint32) || !window.MarkCompleted(0) {
		t.Fatal("valid wraparound sequence was rejected")
	}
	if window.MarkCompleted(math.MaxUint32) {
		t.Fatal("pre-wrap duplicate was accepted")
	}
}
