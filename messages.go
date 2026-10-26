package main

import (
	"database/sql"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

const maxMessageLength = 4000

// Every message query selects these columns so scanMessage can read the row
const messageColumns = `m.id, m.conversation_id, m.sender, m.content, m.created_at, m.edited_at, m.deleted_at,
	a.id, a.filename, a.content_type, a.size`

const messageFrom = `FROM messages m LEFT JOIN attachments a ON a.id = m.attachment_id`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanMessage(s rowScanner) (Message, error) {
	var m Message
	var editedAt, deletedAt sql.NullTime
	var attID, attSize sql.NullInt64
	var attName, attType sql.NullString
	err := s.Scan(&m.ID, &m.ConversationID, &m.Sender, &m.Content, &m.CreatedAt, &editedAt, &deletedAt,
		&attID, &attName, &attType, &attSize)
	if err != nil {
		return m, err
	}
	if editedAt.Valid {
		m.EditedAt = &editedAt.Time
	}
	if deletedAt.Valid {
		m.DeletedAt = &deletedAt.Time
	}
	if attID.Valid {
		m.Attachment = &Attachment{ID: attID.Int64, Filename: attName.String, ContentType: attType.String, Size: attSize.Int64}
	}
	return m, nil
}

func loadMessage(id int64) (Message, error) {
	msg, err := scanMessage(db.QueryRow(`SELECT `+messageColumns+` `+messageFrom+` WHERE m.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return msg, errMessageMissing
	}
	return msg, err
}

// cleanContent trims a message body and checks it's sendable
func cleanContent(content string, hasAttachment bool) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" && !hasAttachment {
		return "", badRequest("message is empty")
	}
	if utf8.RuneCountInString(content) > maxMessageLength {
		return "", badRequest("message is too long")
	}
	return content, nil
}

// insertMessage stores a new message. attachmentID is 0 for plain text.
func insertMessage(convID int64, sender, content string, attachmentID int64) (Message, error) {
	content, err := cleanContent(content, attachmentID != 0)
	if err != nil {
		return Message{}, err
	}

	tx, err := db.Begin()
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()

	var attachment any // stays nil (NULL) for plain text
	if attachmentID != 0 {
		// Only your own upload, and only once
		var ok bool
		err := tx.QueryRow(`
			SELECT COUNT(*) > 0 FROM attachments a
			WHERE a.id = ? AND a.uploader = ?
			  AND NOT EXISTS (SELECT 1 FROM messages WHERE attachment_id = a.id)`,
			attachmentID, sender).Scan(&ok)
		if err != nil {
			return Message{}, err
		}
		if !ok {
			return Message{}, badRequest("attachment not found")
		}
		attachment = attachmentID
	}

	res, err := tx.Exec(
		"INSERT INTO messages (conversation_id, sender, content, attachment_id) VALUES (?, ?, ?, ?)",
		convID, sender, content, attachment,
	)
	if err != nil {
		return Message{}, err
	}
	id, _ := res.LastInsertId()

	// Sending a message means you've read everything before it
	if _, err := tx.Exec(
		"UPDATE conversation_members SET last_read_id = ?, read_at = CURRENT_TIMESTAMP WHERE conversation_id = ? AND username = ?",
		id, convID, sender,
	); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, err
	}
	return loadMessage(id)
}

// messagesPage returns up to limit messages older than beforeID (or the
// newest ones when beforeID is 0), oldest first, and whether older ones exist
func messagesPage(convID, beforeID int64, limit int) ([]Message, bool, error) {
	query := `SELECT ` + messageColumns + ` ` + messageFrom + ` WHERE m.conversation_id = ?`
	args := []any{convID}
	if beforeID > 0 {
		query += ` AND m.id < ?`
		args = append(args, beforeID)
	}
	// Fetch one extra row to learn whether there's another page
	query += ` ORDER BY m.id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	msgs := []Message{}
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return nil, false, err
		}
		msgs = append(msgs, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := len(msgs) > limit
	if hasMore {
		msgs = msgs[:limit]
	}
	slices.Reverse(msgs)
	return msgs, hasMore, nil
}

// ownMessage loads a message the user is allowed to change
func ownMessage(id int64, user string) (Message, error) {
	msg, err := loadMessage(id)
	if err != nil {
		return msg, err
	}
	if msg.Sender != user {
		return msg, errNotSender
	}
	if msg.DeletedAt != nil {
		return msg, badRequest("message was deleted")
	}
	return msg, nil
}

func editMessage(id int64, user, content string) (Message, error) {
	msg, err := ownMessage(id, user)
	if err != nil {
		return msg, err
	}
	content, err = cleanContent(content, msg.Attachment != nil)
	if err != nil {
		return msg, err
	}
	if _, err := db.Exec(
		"UPDATE messages SET content = ?, edited_at = CURRENT_TIMESTAMP WHERE id = ? AND deleted_at IS NULL",
		content, id,
	); err != nil {
		return msg, err
	}
	return loadMessage(id)
}

// deleteMessage blanks a message but keeps its row, so the conversation shows
// "message deleted" in its place and pagination IDs stay stable
func deleteMessage(id int64, user string) (Message, error) {
	msg, err := ownMessage(id, user)
	if err != nil {
		return msg, err
	}
	if _, err := db.Exec(
		"UPDATE messages SET content = '', attachment_id = NULL, deleted_at = CURRENT_TIMESTAMP WHERE id = ?",
		id,
	); err != nil {
		return msg, err
	}
	if msg.Attachment != nil {
		removeAttachments([]int64{msg.Attachment.ID})
	}
	return loadMessage(id)
}

// removeAttachments deletes attachment rows and their files on disk. Failures
// are only logged: the message they belonged to is already gone.
func removeAttachments(ids []int64) {
	for _, id := range ids {
		var stored string
		if err := db.QueryRow("SELECT stored_name FROM attachments WHERE id = ?", id).Scan(&stored); err != nil {
			log.Println("❌ Attachment lookup failed:", id, err)
			continue
		}
		if _, err := db.Exec("DELETE FROM attachments WHERE id = ?", id); err != nil {
			log.Println("❌ Attachment delete failed:", id, err)
			continue
		}
		if err := os.Remove(filepath.Join(uploadDir, stored)); err != nil && !os.IsNotExist(err) {
			log.Println("❌ Could not remove upload:", stored, err)
		}
	}
}
