package run

import "sync"

const (
	hubBuffer = 5000
	subBuffer = 512
)

type StreamEvent struct {
	ID   int
	Type string
	Data any
}

type Hub struct {
	mu     sync.Mutex
	events []StreamEvent
	next   int
	subs   map[chan StreamEvent]struct{}
	closed bool
}

func NewHub() *Hub {
	return &Hub{next: 1, subs: map[chan StreamEvent]struct{}{}}
}

func (h *Hub) Publish(typ string, data any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	ev := StreamEvent{ID: h.next, Type: typ, Data: data}
	h.next++
	h.events = append(h.events, ev)
	if len(h.events) > hubBuffer {
		h.events = append([]StreamEvent(nil), h.events[len(h.events)-hubBuffer:]...)
	}
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for ch := range h.subs {
		close(ch)
	}
	h.subs = map[chan StreamEvent]struct{}{}
}

func (h *Hub) Subscribe(after int) ([]StreamEvent, <-chan StreamEvent, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var replay []StreamEvent
	for _, ev := range h.events {
		if ev.ID > after {
			replay = append(replay, ev)
		}
	}
	ch := make(chan StreamEvent, subBuffer)
	if h.closed {
		close(ch)
		return replay, ch, func() {}
	}
	h.subs[ch] = struct{}{}
	return replay, ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
	}
}
