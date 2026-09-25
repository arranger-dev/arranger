package orch

import (
	"encoding/json"
	"sync"
)

// Hub fans out live updates to subscribers of a project and remembers each agent's latest
// activity line.
type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]*sub
	last map[string]string // agent id -> latest activity
}

type sub struct {
	project string
	dropped bool // a message didn't fit; the page must reload its state
}

func NewHub() *Hub {
	return &Hub{subs: map[chan []byte]*sub{}, last: map[string]string{}}
}

// Subscribe returns a channel of JSON messages for one project.
func (h *Hub) Subscribe(project string) chan []byte {
	c := make(chan []byte, 256)
	h.mu.Lock()
	h.subs[c] = &sub{project: project}
	h.mu.Unlock()
	return c
}

// Dropped reports, once, whether messages to c were dropped since the last call.
func (h *Hub) Dropped(c chan []byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.subs[c]
	if s == nil || !s.dropped {
		return false
	}
	s.dropped = false
	return true
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
	for c, s := range h.subs {
		if s.project != project {
			continue
		}
		select {
		case c <- b:
		default: // a slow page misses this; it's told to reload once it catches up
			s.dropped = true
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
