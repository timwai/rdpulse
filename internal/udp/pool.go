package udp

import "sync"

const (
	// DefaultBufferSize is sized to fit max UDP datagrams comfortably
	DefaultBufferSize = 2048
)

var packetPool = sync.Pool{
	New: func() any {
		b := make([]byte, DefaultBufferSize)
		return &b
	},
}

// GetBuffer retrieves a reusable buffer from the pool
func GetBuffer() *[]byte {
	return packetPool.Get().(*[]byte)
}

// PutBuffer returns a buffer to the pool
func PutBuffer(b *[]byte) {
	if b == nil || cap(*b) < DefaultBufferSize {
		return
	}
	*b = (*b)[:DefaultBufferSize]
	packetPool.Put(b)
}

// LargeBufferSize holds a fully reassembled UDP packet plus any framing header.
const LargeBufferSize = 65535 + 64

var largePacketPool = sync.Pool{
	New: func() any {
		b := make([]byte, LargeBufferSize)
		return &b
	},
}

// GetLargeBuffer retrieves a buffer able to hold a whole reassembled packet.
func GetLargeBuffer() *[]byte {
	return largePacketPool.Get().(*[]byte)
}

// PutLargeBuffer returns a large buffer to the pool.
func PutLargeBuffer(b *[]byte) {
	if b == nil || cap(*b) < LargeBufferSize {
		return
	}
	*b = (*b)[:LargeBufferSize]
	largePacketPool.Put(b)
}
