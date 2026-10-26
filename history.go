package main

import (
	"log"
	"net/http"
	"strconv"
)

const (
	defaultPageSize = 50
	maxPageSize     = 100
)

// GET /conversations/{id}/messages?before_id=123&limit=50
//
// Returns the newest messages, or with before_id the ones just older than it,
// oldest first. has_more says whether there's another page further back.
func messagesHandler(w http.ResponseWriter, r *http.Request, user string) {
	convID, err := pathID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := membersFor(convID, user); err != nil {
		writeError(w, err)
		return
	}

	q := r.URL.Query()
	beforeID, _ := strconv.ParseInt(q.Get("before_id"), 10, 64)
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 {
		limit = defaultPageSize
	}
	limit = min(limit, maxPageSize)

	msgs, hasMore, err := messagesPage(convID, beforeID, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": msgs,
		"has_more": hasMore,
	})
}

// DELETE /conversations/{id}/messages wipes the whole history for everyone.
// Either side of a DM may do it; in a group only its creator can.
func clearHistoryHandler(w http.ResponseWriter, r *http.Request, user string) {
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

	var kind, createdBy string
	if err := db.QueryRow("SELECT kind, created_by FROM conversations WHERE id = ?", convID).Scan(&kind, &createdBy); err != nil {
		writeError(w, err)
		return
	}
	if kind == "group" && createdBy != user {
		writeJSONError(w, "Only the group's creator can clear its history", http.StatusForbidden)
		return
	}

	rows, err := db.Query("SELECT attachment_id FROM messages WHERE conversation_id = ? AND attachment_id IS NOT NULL", convID)
	if err != nil {
		writeError(w, err)
		return
	}
	var attachments []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			attachments = append(attachments, id)
		}
	}
	rows.Close()

	if _, err := db.Exec("DELETE FROM messages WHERE conversation_id = ?", convID); err != nil {
		writeError(w, err)
		return
	}
	removeAttachments(attachments)
	log.Printf("🗑️ %s cleared the history of conversation %d\n", user, convID)

	chatHub.sendToUsers(members, conversationIDEvent{Type: evCleared, ConversationID: convID})
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "success",
		"message": "Chat history deleted",
	})
}
