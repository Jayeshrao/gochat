package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocket upgrader with CORS origin check
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return origin == "http://localhost:8080" ||
			origin == "http://127.0.0.1:8080" ||
			origin == "http://localhost:3000" ||
			origin == "https://myfrontend.com"
	},
}

// Every WebSocket frame, in either direction, is a JSON object whose "type"
// says what it is; the other fields depend on the type.
const (
	// Client → server, and echoed back to the conversation's members
	evMessage = "message" // send a message
	evTyping  = "typing"  // started or stopped typing
	evRead    = "read"    // read up to a message
	evEdit    = "edit"    // change your message's text
	evDelete  = "delete"  // delete your message

	// Server → client only
	evHello        = "hello"        // first frame after connecting
	evPresence     = "presence"     // someone came online or went offline
	evConversation = "conversation" // a conversation was created or its members changed
	evLeft         = "left"         // you left a group (from another tab)
	evCleared      = "cleared"      // a conversation's history was wiped
	evError        = "error"        // your last frame was rejected
)

// inEvent is any frame a client sends. Which fields matter depends on Type:
//
//	{"type": "message", "conversation_id": 1, "content": "hi", "attachment_id": 7}
//	{"type": "typing",  "conversation_id": 1, "typing": true}
//	{"type": "read",    "conversation_id": 1, "message_id": 42}
//	{"type": "edit",    "message_id": 42, "content": "fixed"}
//	{"type": "delete",  "message_id": 42}
type inEvent struct {
	Type           string `json:"type"`
	ConversationID int64  `json:"conversation_id"`
	MessageID      int64  `json:"message_id"`
	Content        string `json:"content"`
	AttachmentID   int64  `json:"attachment_id"`
	Typing         bool   `json:"typing"`
}

type helloEvent struct {
	Type   string   `json:"type"`
	User   string   `json:"user"`
	Online []string `json:"online"`
}

type presenceEvent struct {
	Type   string `json:"type"`
	User   string `json:"user"`
	Online bool   `json:"online"`
}

// messageEvent carries a new, edited or deleted message
type messageEvent struct {
	Type    string  `json:"type"`
	Message Message `json:"message"`
}

type typingEvent struct {
	Type           string `json:"type"`
	ConversationID int64  `json:"conversation_id"`
	User           string `json:"user"`
	Typing         bool   `json:"typing"`
}

type readEvent struct {
	Type           string    `json:"type"`
	ConversationID int64     `json:"conversation_id"`
	User           string    `json:"user"`
	MessageID      int64     `json:"message_id"`
	ReadAt         time.Time `json:"read_at"`
}

type conversationEvent struct {
	Type         string       `json:"type"`
	Conversation Conversation `json:"conversation"`
}

type conversationIDEvent struct {
	Type           string `json:"type"`
	ConversationID int64  `json:"conversation_id"`
}

type errorEvent struct {
	Type  string `json:"type"`
	Error string `json:"error"`
}

// WebSocket handler
func handleWS(w http.ResponseWriter, r *http.Request) {
	// --- Authenticate user via JWT ---
	user, ok := authUser(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// --- Upgrade to WebSocket ---
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	c := &client{user: user, conn: conn, send: make(chan []byte, sendBuffer)}
	cameOnline := chatHub.add(c)
	go c.writePump()
	log.Println("✅", user, "connected via WebSocket")

	c.sendEvent(helloEvent{Type: evHello, User: user, Online: chatHub.onlineUsers()})
	if cameOnline {
		chatHub.broadcast(presenceEvent{Type: evPresence, User: user, Online: true})
	}

	c.readPump()

	if chatHub.remove(c) {
		chatHub.broadcast(presenceEvent{Type: evPresence, User: user, Online: false})
	}
	log.Println("👋", user, "disconnected")
}

// readPump handles this client's frames until the connection drops
func (c *client) readPump() {
	c.conn.SetReadLimit(maxEventSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Println("❌ Read error:", err)
			}
			return
		}

		var ev inEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			c.sendEvent(errorEvent{Type: evError, Error: "invalid event"})
			continue
		}

		if err := c.handleEvent(ev); err != nil {
			var ce *clientError
			if !errors.As(err, &ce) {
				log.Printf("❌ %s event from %s failed: %v\n", ev.Type, c.user, err)
				err = errors.New("something went wrong, please try again")
			}
			c.sendEvent(errorEvent{Type: evError, Error: err.Error()})
		}
	}
}

func (c *client) handleEvent(ev inEvent) error {
	switch ev.Type {
	case evMessage:
		return c.onMessage(ev)
	case evTyping:
		return c.onTyping(ev)
	case evRead:
		return c.onRead(ev)
	case evEdit:
		return c.onEdit(ev)
	case evDelete:
		return c.onDelete(ev)
	default:
		return badRequest(fmt.Sprintf("unknown event type %q", ev.Type))
	}
}

func (c *client) onMessage(ev inEvent) error {
	members, err := membersFor(ev.ConversationID, c.user)
	if err != nil {
		return err
	}
	msg, err := insertMessage(ev.ConversationID, c.user, ev.Content, ev.AttachmentID)
	if err != nil {
		return err
	}
	log.Printf("💾 Message %d saved: %s → conversation %d\n", msg.ID, c.user, msg.ConversationID)

	// The sender gets it back too: that's how their other tabs see it, and how
	// the sending tab learns the message's ID
	chatHub.sendToUsers(members, messageEvent{Type: evMessage, Message: msg})
	return nil
}

func (c *client) onTyping(ev inEvent) error {
	members, err := membersFor(ev.ConversationID, c.user)
	if err != nil {
		return err
	}
	chatHub.sendToUsers(without(members, c.user), typingEvent{
		Type:           evTyping,
		ConversationID: ev.ConversationID,
		User:           c.user,
		Typing:         ev.Typing,
	})
	return nil
}

func (c *client) onRead(ev inEvent) error {
	members, err := membersFor(ev.ConversationID, c.user)
	if err != nil {
		return err
	}

	// Clamp to a real message in this conversation
	var upTo int64
	if err := db.QueryRow(
		"SELECT COALESCE(MAX(id), 0) FROM messages WHERE conversation_id = ? AND id <= ?",
		ev.ConversationID, ev.MessageID,
	).Scan(&upTo); err != nil {
		return err
	}

	// Read positions only move forward
	res, err := db.Exec(`
		UPDATE conversation_members SET last_read_id = ?, read_at = CURRENT_TIMESTAMP
		WHERE conversation_id = ? AND username = ? AND last_read_id < ?`,
		upTo, ev.ConversationID, c.user, upTo)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}

	// Everyone gets it: others for read receipts, the reader's other tabs to clear unread counts
	chatHub.sendToUsers(members, readEvent{
		Type:           evRead,
		ConversationID: ev.ConversationID,
		User:           c.user,
		MessageID:      upTo,
		ReadAt:         time.Now().UTC(),
	})
	return nil
}

func (c *client) onEdit(ev inEvent) error {
	return c.changeMessage(ev.MessageID, evEdit, func() (Message, error) {
		return editMessage(ev.MessageID, c.user, ev.Content)
	})
}

func (c *client) onDelete(ev inEvent) error {
	return c.changeMessage(ev.MessageID, evDelete, func() (Message, error) {
		return deleteMessage(ev.MessageID, c.user)
	})
}

// changeMessage applies an edit or delete and tells the conversation about it
func (c *client) changeMessage(id int64, evType string, change func() (Message, error)) error {
	before, err := loadMessage(id)
	if err != nil {
		return err
	}
	members, err := membersFor(before.ConversationID, c.user)
	if err != nil {
		return err
	}
	msg, err := change()
	if err != nil {
		return err
	}
	log.Printf("✏️ %s %s message %d\n", c.user, evType, msg.ID)
	chatHub.sendToUsers(members, messageEvent{Type: evType, Message: msg})
	return nil
}
