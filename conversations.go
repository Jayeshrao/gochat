package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const maxGroupNameLength = 64

// dmKey identifies the DM between two users no matter who started it
func dmKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	key, _ := json.Marshal([]string{a, b})
	return string(key)
}

// membersFor returns everyone in the conversation, or errNotMember if user
// isn't one of them. Use it to authorize anything done to a conversation.
func membersFor(convID int64, user string) ([]string, error) {
	rows, err := db.Query("SELECT username FROM conversation_members WHERE conversation_id = ?", convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		members = append(members, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !slices.Contains(members, user) {
		return nil, errNotMember
	}
	return members, nil
}

func without(users []string, user string) []string {
	return slices.DeleteFunc(slices.Clone(users), func(u string) bool { return u == user })
}

// requireUsersExist returns a clientError naming the first unknown user
func requireUsersExist(names []string) error {
	for _, name := range names {
		var exists bool
		if err := db.QueryRow("SELECT COUNT(*) > 0 FROM users WHERE username = ?", name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return &clientError{http.StatusNotFound, fmt.Sprintf("user %q not found", name)}
		}
	}
	return nil
}

// queryConversations loads the user's conversations with members, last
// message and unread count. onlyID limits it to one conversation; 0 means all.
func queryConversations(user string, onlyID int64) ([]Conversation, error) {
	rows, err := db.Query(`
		SELECT c.id, c.kind, c.name, c.created_by, c.created_at,
			(SELECT COUNT(*) FROM messages m
			 WHERE m.conversation_id = c.id AND m.id > me.last_read_id
			   AND m.sender <> me.username AND m.deleted_at IS NULL)
		FROM conversations c
		JOIN conversation_members me ON me.conversation_id = c.id AND me.username = ?
		WHERE ? = 0 OR c.id = ?`,
		user, onlyID, onlyID)
	if err != nil {
		return nil, err
	}
	convs := []Conversation{}
	index := map[int64]int{}
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.Kind, &c.Name, &c.CreatedBy, &c.CreatedAt, &c.UnreadCount); err != nil {
			rows.Close()
			return nil, err
		}
		c.Members = []Member{}
		index[c.ID] = len(convs)
		convs = append(convs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(convs) == 0 {
		return convs, nil
	}

	// Everyone's read position, for read receipts
	rows, err = db.Query(`
		SELECT cm.conversation_id, cm.username, cm.last_read_id, cm.read_at
		FROM conversation_members cm
		JOIN conversation_members me ON me.conversation_id = cm.conversation_id AND me.username = ?
		WHERE ? = 0 OR cm.conversation_id = ?
		ORDER BY cm.joined_at, cm.username`,
		user, onlyID, onlyID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var convID int64
		var m Member
		var readAt sql.NullTime
		if err := rows.Scan(&convID, &m.Username, &m.LastReadID, &readAt); err != nil {
			rows.Close()
			return nil, err
		}
		if readAt.Valid {
			m.ReadAt = &readAt.Time
		}
		if i, ok := index[convID]; ok {
			convs[i].Members = append(convs[i].Members, m)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The newest message of each conversation, for the sidebar preview
	rows, err = db.Query(`SELECT `+messageColumns+` `+messageFrom+`
		WHERE m.id IN (
			SELECT MAX(m2.id) FROM messages m2
			JOIN conversation_members me ON me.conversation_id = m2.conversation_id AND me.username = ?
			WHERE ? = 0 OR m2.conversation_id = ?
			GROUP BY m2.conversation_id)`,
		user, onlyID, onlyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		if i, ok := index[msg.ConversationID]; ok {
			convs[i].LastMessage = &msg
		}
	}
	return convs, rows.Err()
}

func loadConversation(convID int64, user string) (Conversation, error) {
	convs, err := queryConversations(user, convID)
	if err != nil {
		return Conversation{}, err
	}
	if len(convs) == 0 {
		return Conversation{}, errNotMember
	}
	return convs[0], nil
}

// lastActivity is when the conversation last changed, for sorting the sidebar
func (c Conversation) lastActivity() time.Time {
	if c.LastMessage != nil {
		return c.LastMessage.CreatedAt
	}
	return c.CreatedAt
}

// pushConversation sends each user their own view of the conversation (unread
// counts differ per member) so their sidebar picks up new groups and members
func pushConversation(convID int64, users []string) {
	for _, u := range users {
		conv, err := loadConversation(convID, u)
		if err != nil {
			log.Println("❌ Could not load conversation for push:", err)
			continue
		}
		chatHub.sendToUsers([]string{u}, conversationEvent{Type: evConversation, Conversation: conv})
	}
}

// GET /conversations — the sidebar: most recently active first
func conversationsHandler(w http.ResponseWriter, r *http.Request, user string) {
	all, err := queryConversations(user, 0)
	if err != nil {
		writeError(w, err)
		return
	}
	// A DM nobody has written in yet is just someone clicking on a name
	convs := slices.DeleteFunc(all, func(c Conversation) bool {
		return c.Kind == "dm" && c.LastMessage == nil
	})
	sort.SliceStable(convs, func(i, j int) bool {
		ti, tj := convs[i].lastActivity(), convs[j].lastActivity()
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return convs[i].ID > convs[j].ID
	})
	writeJSON(w, http.StatusOK, map[string]any{"conversations": convs})
}

// POST /conversations
//
//	{"kind": "dm", "username": "bob"}                          opens (or creates) the DM with bob
//	{"kind": "group", "name": "Team", "members": ["bob", ...]} creates a group
func createConversationHandler(w http.ResponseWriter, r *http.Request, user string) {
	var req struct {
		Kind     string   `json:"kind"`
		Username string   `json:"username"`
		Name     string   `json:"name"`
		Members  []string `json:"members"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	switch req.Kind {
	case "dm":
		convID, err := openDM(user, strings.TrimSpace(req.Username))
		if err != nil {
			writeError(w, err)
			return
		}
		conv, err := loadConversation(convID, user)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, conv)

	case "group":
		convID, members, err := createGroup(user, req.Name, req.Members)
		if err != nil {
			writeError(w, err)
			return
		}
		log.Printf("👥 %s created group %d with %v\n", user, convID, members)
		pushConversation(convID, members)
		conv, err := loadConversation(convID, user)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, conv)

	default:
		writeJSONError(w, `kind must be "dm" or "group"`, http.StatusBadRequest)
	}
}

// openDM returns the DM between user and other, creating it on first use
func openDM(user, other string) (int64, error) {
	if other == "" {
		return 0, badRequest("username is required")
	}
	if other == user {
		return 0, badRequest("you can't start a conversation with yourself")
	}
	if err := requireUsersExist([]string{other}); err != nil {
		return 0, err
	}

	key := dmKey(user, other)
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		"INSERT INTO conversations (kind, dm_key, created_by) VALUES ('dm', ?, ?) ON CONFLICT (dm_key) DO NOTHING",
		key, user,
	); err != nil {
		return 0, err
	}
	var convID int64
	if err := tx.QueryRow("SELECT id FROM conversations WHERE dm_key = ?", key).Scan(&convID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		"INSERT OR IGNORE INTO conversation_members (conversation_id, username) VALUES (?, ?), (?, ?)",
		convID, user, convID, other,
	); err != nil {
		return 0, err
	}
	return convID, tx.Commit()
}

// cleanUsernames trims, drops blanks and duplicates, and leaves out skip
func cleanUsernames(names []string, skip []string) []string {
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n != "" && !slices.Contains(skip, n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func createGroup(creator, name string, invited []string) (int64, []string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, nil, badRequest("group name is required")
	}
	if utf8.RuneCountInString(name) > maxGroupNameLength {
		return 0, nil, badRequest("group name is too long")
	}
	others := cleanUsernames(invited, []string{creator})
	if len(others) == 0 {
		return 0, nil, badRequest("add at least one other member")
	}
	if err := requireUsersExist(others); err != nil {
		return 0, nil, err
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()

	res, err := tx.Exec("INSERT INTO conversations (kind, name, created_by) VALUES ('group', ?, ?)", name, creator)
	if err != nil {
		return 0, nil, err
	}
	convID, _ := res.LastInsertId()

	members := append([]string{creator}, others...)
	for _, m := range members {
		if _, err := tx.Exec("INSERT INTO conversation_members (conversation_id, username) VALUES (?, ?)", convID, m); err != nil {
			return 0, nil, err
		}
	}
	return convID, members, tx.Commit()
}

// requireGroup fails unless the conversation is a group
func requireGroup(convID int64) error {
	var kind string
	if err := db.QueryRow("SELECT kind FROM conversations WHERE id = ?", convID).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotMember
		}
		return err
	}
	if kind != "group" {
		return badRequest("only groups have members to add or leave")
	}
	return nil
}

// POST /conversations/{id}/members {"usernames": ["carol"]}
func addMembersHandler(w http.ResponseWriter, r *http.Request, user string) {
	convID, err := pathID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req struct {
		Usernames []string `json:"usernames"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	members, err := membersFor(convID, user)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := requireGroup(convID); err != nil {
		writeError(w, err)
		return
	}
	added := cleanUsernames(req.Usernames, members)
	if len(added) == 0 {
		writeJSONError(w, "No new members to add", http.StatusBadRequest)
		return
	}
	if err := requireUsersExist(added); err != nil {
		writeError(w, err)
		return
	}

	for _, u := range added {
		// New members start caught up, so the history they can scroll back
		// through doesn't all count as unread
		if _, err := db.Exec(`
			INSERT OR IGNORE INTO conversation_members (conversation_id, username, last_read_id)
			VALUES (?, ?, (SELECT COALESCE(MAX(id), 0) FROM messages WHERE conversation_id = ?))`,
			convID, u, convID); err != nil {
			writeError(w, err)
			return
		}
	}
	log.Printf("👥 %s added %v to group %d\n", user, added, convID)

	pushConversation(convID, append(members, added...))
	conv, err := loadConversation(convID, user)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

// POST /conversations/{id}/leave
func leaveConversationHandler(w http.ResponseWriter, r *http.Request, user string) {
	convID, err := pathID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	members, err := membersFor(convID, user)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := requireGroup(convID); err != nil {
		writeError(w, err)
		return
	}
	if _, err := db.Exec("DELETE FROM conversation_members WHERE conversation_id = ? AND username = ?", convID, user); err != nil {
		writeError(w, err)
		return
	}
	log.Printf("👋 %s left group %d\n", user, convID)

	pushConversation(convID, without(members, user))
	// The user's other tabs drop it from their sidebar
	chatHub.sendToUsers([]string{user}, conversationIDEvent{Type: evLeft, ConversationID: convID})
	writeJSON(w, http.StatusOK, map[string]string{"status": "left"})
}
