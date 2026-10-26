package main

import "time"

// Represents a user in the system
type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Password string `json:"-"` // hide in JSON responses
}

// A conversation is either a 1:1 DM or a named group; every message belongs to one
type Conversation struct {
	ID          int64     `json:"id"`
	Kind        string    `json:"kind"` // "dm" or "group"
	Name        string    `json:"name,omitempty"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	Members     []Member  `json:"members"`
	LastMessage *Message  `json:"last_message,omitempty"`
	UnreadCount int       `json:"unread_count"` // for the user the conversation was loaded for
}

// A member's read position drives both unread counts and read receipts
type Member struct {
	Username   string     `json:"username"`
	LastReadID int64      `json:"last_read_id"`
	ReadAt     *time.Time `json:"read_at,omitempty"`
}

// Represents a chat message
type Message struct {
	ID             int64       `json:"id"`
	ConversationID int64       `json:"conversation_id"`
	Sender         string      `json:"sender"`
	Content        string      `json:"content"`
	Attachment     *Attachment `json:"attachment,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	EditedAt       *time.Time  `json:"edited_at,omitempty"`
	DeletedAt      *time.Time  `json:"deleted_at,omitempty"`
}

// An uploaded image or file
type Attachment struct {
	ID          int64  `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}
