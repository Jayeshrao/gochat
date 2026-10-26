package main

import (
	"database/sql"
	"log"
	"path/filepath"

	_ "modernc.org/sqlite"
)

var db *sql.DB

func initDB() {
	var err error
	// busy_timeout makes concurrent writers wait instead of failing with "database is locked",
	// and _txlock=immediate takes the write lock at BEGIN so two transactions can't deadlock.
	db, err = sql.Open("sqlite", "file:./chatapp.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		log.Fatal(err)
	}

	// Print the exact DB path being used
	absPath, _ := filepath.Abs("./chatapp.db")
	log.Println("📂 Using DB file at:", absPath)

	if err := migrate(); err != nil {
		log.Fatal("Error migrating database: ", err)
	}
}

// migrate brings the schema up to date. PRAGMA user_version records which
// migrations have already run, so each one runs exactly once.
func migrate() error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 1 {
		if err := migrateToConversations(); err != nil {
			return err
		}
	}
	return nil
}

var schemaV1 = []string{
	`CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password TEXT NOT NULL
	)`,
	`CREATE TABLE conversations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL CHECK (kind IN ('dm', 'group')),
		name TEXT NOT NULL DEFAULT '',
		dm_key TEXT UNIQUE, -- the sorted pair of usernames for a DM, NULL for groups
		created_by TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE conversation_members (
		conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		username TEXT NOT NULL,
		last_read_id INTEGER NOT NULL DEFAULT 0, -- newest message this member has read
		read_at TIMESTAMP,                       -- when last_read_id last moved
		joined_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (conversation_id, username)
	)`,
	`CREATE INDEX idx_members_username ON conversation_members(username)`,
	`CREATE TABLE attachments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		uploader TEXT NOT NULL,
		filename TEXT NOT NULL,
		content_type TEXT NOT NULL,
		size INTEGER NOT NULL,
		stored_name TEXT NOT NULL, -- file name inside uploadDir
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		sender TEXT NOT NULL,
		content TEXT NOT NULL DEFAULT '',
		attachment_id INTEGER REFERENCES attachments(id) ON DELETE SET NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		edited_at TIMESTAMP,
		deleted_at TIMESTAMP
	)`,
	`CREATE INDEX idx_messages_conversation ON messages(conversation_id, id)`,
}

// migrateToConversations replaces the old sender/receiver messages table with
// conversations. Existing DMs between registered users are carried over with
// their original IDs; the old table is kept as messages_v0 in case anything
// needs to be recovered from it.
func migrateToConversations() error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var legacy int
	if err := tx.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'messages'").Scan(&legacy); err != nil {
		return err
	}
	if legacy > 0 {
		if _, err := tx.Exec("ALTER TABLE messages RENAME TO messages_v0"); err != nil {
			return err
		}
	}

	for _, stmt := range schemaV1 {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}

	if legacy > 0 {
		if err := copyLegacyMessages(tx); err != nil {
			return err
		}
	}

	if _, err := tx.Exec("PRAGMA user_version = 1"); err != nil {
		return err
	}
	return tx.Commit()
}

func copyLegacyMessages(tx *sql.Tx) error {
	// Only pairs of registered users are carried over; rows with a blank or
	// unknown receiver were never reachable from the app.
	rows, err := tx.Query(`
		SELECT DISTINCT m.sender, m.receiver
		FROM messages_v0 m
		JOIN users s ON s.username = m.sender
		JOIN users r ON r.username = m.receiver
		WHERE m.sender <> m.receiver`)
	if err != nil {
		return err
	}
	pairs := map[string][2]string{}
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			rows.Close()
			return err
		}
		if a > b {
			a, b = b, a
		}
		pairs[dmKey(a, b)] = [2]string{a, b}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	copied := 0
	for key, pair := range pairs {
		a, b := pair[0], pair[1]
		res, err := tx.Exec("INSERT INTO conversations (kind, dm_key, created_by) VALUES ('dm', ?, ?)", key, a)
		if err != nil {
			return err
		}
		convID, _ := res.LastInsertId()

		res, err = tx.Exec(`
			INSERT INTO messages (id, conversation_id, sender, content, created_at)
			SELECT id, ?, sender, content, created_at FROM messages_v0
			WHERE (sender = ? AND receiver = ?) OR (sender = ? AND receiver = ?)`,
			convID, a, b, b, a)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		copied += int(n)

		// Treat existing history as already read so it doesn't all show up as unread
		for _, member := range pair {
			if _, err := tx.Exec(`
				INSERT INTO conversation_members (conversation_id, username, last_read_id)
				VALUES (?, ?, (SELECT MAX(id) FROM messages WHERE conversation_id = ?))`,
				convID, member, convID); err != nil {
				return err
			}
		}
	}

	log.Printf("🛠️ Migrated %d messages into %d conversations (originals kept in messages_v0)\n", copied, len(pairs))
	return nil
}
