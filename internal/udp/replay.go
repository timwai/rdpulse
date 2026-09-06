package udp

import "sync"

// ReplayWindow tracks recently completed packet IDs using serial-number
// arithmetic. A packet is marked only after all of its fragments have been
// authenticated and reassembled, so fragments sharing an ID are not rejected.
type ReplayWindow struct {
	mu          sync.Mutex
	initialized bool
	highest     uint32
	bitmap      uint64
}

// Seen reports whether packetID has already completed or is too old for the
// 64-packet receive window.
func (w *ReplayWindow) Seen(packetID uint32) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seenLocked(packetID)
}

// MarkCompleted records a fully reassembled packet. It returns false when the
// packet was already delivered or fell outside the replay window.
func (w *ReplayWindow) MarkCompleted(packetID uint32) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.initialized {
		w.initialized = true
		w.highest = packetID
		w.bitmap = 1
		return true
	}

	delta := int32(packetID - w.highest)
	if delta > 0 {
		shift := uint32(delta)
		if shift >= 64 {
			w.bitmap = 1
		} else {
			w.bitmap = (w.bitmap << shift) | 1
		}
		w.highest = packetID
		return true
	}

	behind := uint32(-delta)
	if behind >= 64 {
		return false
	}
	mask := uint64(1) << behind
	if w.bitmap&mask != 0 {
		return false
	}
	w.bitmap |= mask
	return true
}

func (w *ReplayWindow) seenLocked(packetID uint32) bool {
	if !w.initialized {
		return false
	}
	delta := int32(packetID - w.highest)
	if delta > 0 {
		return false
	}
	behind := uint32(-delta)
	return behind >= 64 || w.bitmap&(uint64(1)<<behind) != 0
}
