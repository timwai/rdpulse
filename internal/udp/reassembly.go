package udp

import (
	"context"
	"errors"
	"sync"
	"time"

	"rdpulse/internal/protocol"
)

var (
	ErrFragmentExpired   = errors.New("fragment expired")
	ErrReassemblyLimit   = errors.New("reassembly limit exceeded")
	ErrCorruptPacketSize = errors.New("packet exceeds max allowed size")
)

const (
	DefaultReassemblyTimeout = 100 * time.Millisecond
	MaxReassemblyPerSession  = 32
	MaxGlobalReassemblyCount = 1024
)

type reassemblyKey struct {
	sessionID uint32
	packetID  uint32
}

// packetReassembly uses fixed-size fragment arrays so in-flight packets can be
// pooled, keeping multi-fragment reassembly off the allocator.
type packetReassembly struct {
	sessionID     uint32
	packetID      uint32
	fragmentCount uint8
	receivedCount uint8
	totalBytes    int
	fragments     [protocol.MaxUDPFragments][]byte
	fragBufs      [protocol.MaxUDPFragments]*[]byte
	createdAt     time.Time
}

var reassemblyPool = sync.Pool{New: func() any { return new(packetReassembly) }}

// releaseFragments returns pooled fragment copies so a completed or expired
// packet does not keep them alive.
func (p *packetReassembly) releaseFragments() {
	for i := 0; i < int(p.fragmentCount); i++ {
		if buf := p.fragBufs[i]; buf != nil {
			PutBuffer(buf)
			p.fragBufs[i] = nil
		}
		p.fragments[i] = nil
	}
}

func (p *packetReassembly) recycle() {
	p.releaseFragments()
	p.receivedCount = 0
	p.totalBytes = 0
	p.fragmentCount = 0
	reassemblyPool.Put(p)
}

// Reassembler manages datagram fragments reassembly with timeout cleanup
type Reassembler struct {
	mu           sync.Mutex
	timeout      time.Duration
	pending      map[reassemblyKey]*packetReassembly
	sessionCount map[uint32]int
	closed       bool
}

// NewReassembler creates a new Reassembler
func NewReassembler(timeout time.Duration) *Reassembler {
	if timeout <= 0 {
		timeout = DefaultReassemblyTimeout
	}
	r := &Reassembler{
		timeout:      timeout,
		pending:      make(map[reassemblyKey]*packetReassembly),
		sessionCount: make(map[uint32]int),
	}
	return r
}

// Feed receives a fragment. If this completes the packet, the assembled payload is returned.
// Otherwise returns (nil, nil) if more fragments are needed, or an error if invalid.
// The assembled packet is freshly allocated; use FeedAppend on hot paths.
func (r *Reassembler) Feed(hdr *protocol.UDPHeader, payload []byte) ([]byte, error) {
	return r.FeedAppend(nil, hdr, payload)
}

// FeedAppend is Feed writing the assembled packet into dst. Callers that reuse
// dst across packets keep the receive path allocation-free. The returned slice
// is only valid until the next call that reuses the same dst.
func (r *Reassembler) FeedAppend(dst []byte, hdr *protocol.UDPHeader, payload []byte) ([]byte, error) {
	// Fast-path: single fragment packet
	if hdr.FragmentCount == 1 && hdr.FragmentID == 0 {
		return append(dst[:0], payload...), nil
	}

	if hdr.FragmentCount == 0 || hdr.FragmentID >= hdr.FragmentCount || hdr.FragmentCount > protocol.MaxUDPFragments {
		return nil, protocol.ErrUDPInvalidFragment
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, errors.New("reassembler closed")
	}

	key := reassemblyKey{sessionID: hdr.SessionID, packetID: hdr.PacketID}
	entry, exists := r.pending[key]
	if !exists {
		if len(r.pending) >= MaxGlobalReassemblyCount {
			return nil, ErrReassemblyLimit
		}
		if r.sessionCount[hdr.SessionID] >= MaxReassemblyPerSession {
			return nil, ErrReassemblyLimit
		}

		entry = reassemblyPool.Get().(*packetReassembly)
		entry.sessionID = hdr.SessionID
		entry.packetID = hdr.PacketID
		entry.fragmentCount = hdr.FragmentCount
		entry.receivedCount = 0
		entry.totalBytes = 0
		entry.createdAt = time.Now()
		r.pending[key] = entry
		r.sessionCount[hdr.SessionID]++
	}

	// Check for mismatch in fragment count
	if entry.fragmentCount != hdr.FragmentCount {
		r.deleteEntryLocked(key, entry)
		return nil, protocol.ErrUDPInvalidFragment
	}

	// Deduplicate duplicate fragments
	if entry.fragments[hdr.FragmentID] == nil {
		if len(payload) <= DefaultBufferSize {
			bufPtr := GetBuffer()
			entry.fragBufs[hdr.FragmentID] = bufPtr
			entry.fragments[hdr.FragmentID] = append((*bufPtr)[:0], payload...)
		} else {
			entry.fragments[hdr.FragmentID] = append([]byte(nil), payload...)
		}
		entry.receivedCount++
		entry.totalBytes += len(payload)

		if entry.totalBytes > protocol.MaxUDPPacketSize {
			r.deleteEntryLocked(key, entry)
			return nil, ErrCorruptPacketSize
		}
	}

	// If all fragments arrived, assemble in order
	if entry.receivedCount == entry.fragmentCount {
		assembled := dst[:0]
		for i := 0; i < int(entry.fragmentCount); i++ {
			assembled = append(assembled, entry.fragments[i]...)
		}
		r.deleteEntryLocked(key, entry)
		return assembled, nil
	}

	return nil, nil
}

func (r *Reassembler) deleteEntryLocked(key reassemblyKey, entry *packetReassembly) {
	delete(r.pending, key)
	defer entry.recycle()
	if count := r.sessionCount[entry.sessionID]; count > 1 {
		r.sessionCount[entry.sessionID] = count - 1
	} else {
		delete(r.sessionCount, entry.sessionID)
	}
}

// StartCleaner runs a periodic sweeper to drop incomplete packets exceeding the timeout
func (r *Reassembler) StartCleaner(ctx context.Context, checkInterval time.Duration) {
	if checkInterval <= 0 {
		checkInterval = 50 * time.Millisecond
	}
	ticker := time.NewTicker(checkInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				r.mu.Lock()
				r.closed = true
				for _, entry := range r.pending {
					entry.recycle()
				}
				r.pending = nil
				r.sessionCount = nil
				r.mu.Unlock()
				return
			case now := <-ticker.C:
				r.mu.Lock()
				deadline := now.Add(-r.timeout)
				for k, entry := range r.pending {
					if entry.createdAt.Before(deadline) {
						r.deleteEntryLocked(k, entry)
					}
				}
				r.mu.Unlock()
			}
		}
	}()
}
