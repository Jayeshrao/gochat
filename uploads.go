package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	uploadDir     = "./uploads"
	maxUploadSize = 10 << 20 // 10 MB
)

// Image types the browser may show inline. Everything else, SVG and HTML
// included, is only ever served as a download so it can't run scripts on our origin.
var inlineImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// POST /upload (multipart, field "file") stores the file and returns its
// attachment. Send it with {"type": "message", "attachment_id": id}.
func uploadHandler(w http.ResponseWriter, r *http.Request, user string) {
	// Leave room for the multipart framing around the file itself
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize+1<<20)
	file, header, err := r.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSONError(w, "File is larger than 10 MB", http.StatusRequestEntityTooLarge)
			return
		}
		writeJSONError(w, `Expected a multipart form with a "file" field`, http.StatusBadRequest)
		return
	}
	defer file.Close()
	if header.Size > maxUploadSize {
		writeJSONError(w, "File is larger than 10 MB", http.StatusRequestEntityTooLarge)
		return
	}

	// Trust the bytes, not the browser's claimed type
	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	contentType := http.DetectContentType(head[:n])
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, err)
		return
	}

	storedName, err := randomName()
	if err != nil {
		writeError(w, err)
		return
	}
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		writeError(w, err)
		return
	}
	path := filepath.Join(uploadDir, storedName)
	dst, err := os.Create(path)
	if err != nil {
		writeError(w, err)
		return
	}
	size, err := io.Copy(dst, file)
	if closeErr := dst.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path)
		writeError(w, err)
		return
	}

	filename := cleanFilename(header.Filename)
	res, err := db.Exec(
		"INSERT INTO attachments (uploader, filename, content_type, size, stored_name) VALUES (?, ?, ?, ?, ?)",
		user, filename, contentType, size, storedName,
	)
	if err != nil {
		os.Remove(path)
		writeError(w, err)
		return
	}
	id, _ := res.LastInsertId()
	log.Printf("📎 %s uploaded %q (%d bytes, %s)\n", user, filename, size, contentType)

	writeJSON(w, http.StatusCreated, Attachment{ID: id, Filename: filename, ContentType: contentType, Size: size})
}

// GET /attachments/{id} serves a file to its uploader and to members of the
// conversation it was posted in. Add ?download=1 to force a download.
func attachmentHandler(w http.ResponseWriter, r *http.Request, user string) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, err)
		return
	}

	var a Attachment
	var uploader, storedName string
	err = db.QueryRow(
		"SELECT filename, content_type, uploader, stored_name FROM attachments WHERE id = ?", id,
	).Scan(&a.Filename, &a.ContentType, &uploader, &storedName)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		writeError(w, err)
		return
	}

	allowed := uploader == user
	if !allowed {
		err := db.QueryRow(`
			SELECT COUNT(*) > 0 FROM messages m
			JOIN conversation_members cm ON cm.conversation_id = m.conversation_id AND cm.username = ?
			WHERE m.attachment_id = ?`, user, id).Scan(&allowed)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	if !allowed {
		// Same answer as a missing file, so IDs can't be probed
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(filepath.Join(uploadDir, storedName))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, err)
		return
	}

	disposition := "attachment"
	if inlineImageTypes[a.ContentType] && r.URL.Query().Get("download") == "" {
		disposition = "inline"
	}
	if header := mime.FormatMediaType(disposition, map[string]string{"filename": a.Filename}); header != "" {
		w.Header().Set("Content-Disposition", header)
	} else {
		w.Header().Set("Content-Disposition", disposition)
	}
	w.Header().Set("Content-Type", a.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func randomName() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// cleanFilename keeps the name the user sees short and free of path parts
func cleanFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == "/" {
		return "file"
	}
	if r := []rune(name); len(r) > 200 {
		name = string(r[:200])
	}
	return name
}
