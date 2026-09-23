package orch

import (
	"encoding/json"
	"sync"
)

// Hub fans out live updates to subscribers of a project and remembers each agent's latest
// activity line.
type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]string // chan -> project id
	last map[string]string      // agent id -> latest activity
}

func NewHub() *Hub {
	return &Hub{subs: map[chan []byte]string{}, last: map[string]string{}}
}

// Subscribe returns a channel of JSON messages for one project.
func (h *Hub) Subscribe(project string) chan []byte {
	c := make(chan []byte, 256)
	h.mu.Lock()
	h.subs[c] = project
	h.mu.Unlock()
	return c
}

func (h *Hub) Unsubscribe(c chan []byte) {
	h.mu.Lock()
	delete(h.subs, c)
	h.mu.Unlock()
}

// Publish sends v as JSON to the project's subscribers.
func (h *Hub) Publish(project string, v any) {
	b, _ := json.Marshal(v)
	h.mu.Lock()
	defer h.mu.Unlock()
	for c, p := range h.subs {
		if p != project {
			continue
		}
		select {
		case c <- b:
		default: // ponytail: slow client drops live events; the Logs tab refetches on open
		}
	}
}

func (h *Hub) SetNow(agent, text string) {
	h.mu.Lock()
	h.last[agent] = text
	h.mu.Unlock()
}

// Now is the agent's latest activity line, or "".
func (h *Hub) Now(agent string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last[agent]
}
