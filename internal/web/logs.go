package web

import (
	"sync"
	"time"
)

type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}

type LogHub struct {
	mu      sync.RWMutex
	history []LogEntry
	maxSize int
	clients map[chan LogEntry]struct{}
}

var Hub = NewLogHub(200)

func NewLogHub(maxSize int) *LogHub {
	return &LogHub{
		history: make([]LogEntry, 0, maxSize),
		maxSize: maxSize,
		clients: make(map[chan LogEntry]struct{}),
	}
}

func (h *LogHub) Write(p []byte) (n int, err error) {
	msg := string(p)
	entry := LogEntry{
		Timestamp: time.Now().Format("15:04:05"),
		Message:   msg,
	}

	h.mu.Lock()
	if len(h.history) >= h.maxSize {
		h.history = h.history[1:]
	}
	h.history = append(h.history, entry)

	for ch := range h.clients {
		select {
		case ch <- entry:
		default:
		}
	}
	h.mu.Unlock()

	return len(p), nil
}

func (h *LogHub) Subscribe() (chan LogEntry, []LogEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()

	ch := make(chan LogEntry, 50)
	h.clients[ch] = struct{}{}

	historyCopy := make([]LogEntry, len(h.history))
	copy(historyCopy, h.history)

	return ch, historyCopy
}

func (h *LogHub) Unsubscribe(ch chan LogEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, exists := h.clients[ch]; exists {
		delete(h.clients, ch)
		close(ch)
	}
}
