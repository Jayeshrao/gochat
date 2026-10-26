package main

import (
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait    = 10 * time.Second
	pongWait     = 60 * time.Second  // drop connections that stop answering pings
	pingPeriod   = pongWait * 9 / 10 // must be shorter than pongWait
	maxEventSize = 64 * 1024         // largest frame a client may send
	sendBuffer   = 64                // events queued per connection before it counts as stuck
)

// client is one WebSocket connection. A user can have several, e.g. two tabs.
// Only writePump writes to conn; everyone else queues frames on send.
type client struct {
	user string
	conn *websocket.Conn
	send chan []byte
}

// hub tracks every live connection so events can be routed to users
type hub struct {
	mu    sync.RWMutex
	conns map[string]map[*client]bool
}

var chatHub = &hub{conns: make(map[string]map[*client]bool)}

// add registers c and reports whether it is the user's first connection,
// i.e. whether they just came online
func (h *hub) add(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[c.user] == nil {
		h.conns[c.user] = make(map[*client]bool)
	}
	h.conns[c.user][c] = true
	return len(h.conns[c.user]) == 1
}

// remove unregisters c and reports whether the user has no connections left,
// i.e. whether they just went offline
func (h *hub) remove(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.conns[c.user]
	if !set[c] {
		return false
	}
	delete(set, c)
	// Closing under the write lock means no sender (they hold the read lock) can hit a closed channel
	close(c.send)
	if len(set) == 0 {
		delete(h.conns, c.user)
		return true
	}
	return false
}

func (h *hub) isOnline(user string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns[user]) > 0
}

func (h *hub) onlineUsers() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	users := make([]string, 0, len(h.conns))
	for u := range h.conns {
		users = append(users, u)
	}
	sort.Strings(users)
	return users
}

// sendToUsers delivers an event to every connection of each listed user
func (h *hub) sendToUsers(users []string, event any) {
	payload, err := json.Marshal(event)
	if err != nil {
		log.Println("❌ Could not encode event:", err)
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, u := range users {
		for c := range h.conns[u] {
			c.queue(payload)
		}
	}
}

// broadcast delivers an event to everyone connected
func (h *hub) broadcast(event any) {
	h.sendToUsers(h.onlineUsers(), event)
}

// queue hands a frame to the connection's writer without blocking. Callers
// must hold chatHub.mu or be the goroutine that will later call remove(c).
func (c *client) queue(payload []byte) {
	select {
	case c.send <- payload:
	default:
		// The client isn't keeping up; drop it rather than stall everyone else.
		// Closing the socket ends its readPump, which unregisters it.
		log.Printf("⚠️ %s is not reading, disconnecting\n", c.user)
		c.conn.Close()
	}
}

// sendEvent queues an event for this connection only. Only called from the
// connection's own handler goroutine, which is the one that removes it.
func (c *client) sendEvent(event any) {
	payload, err := json.Marshal(event)
	if err != nil {
		log.Println("❌ Could not encode event:", err)
		return
	}
	c.queue(payload)
}

// writePump is the only goroutine that writes to the socket. It sends queued
// frames and pings so dead connections are noticed and marked offline.
func (c *client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case payload, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel: the connection is going away
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
