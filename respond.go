package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
)

// clientError is a failure caused by the request rather than the server. Its
// message is safe to show the user; code is the HTTP status to answer with.
type clientError struct {
	code int
	msg  string
}

func (e *clientError) Error() string { return e.msg }

func badRequest(msg string) error { return &clientError{http.StatusBadRequest, msg} }

var (
	errNotMember      = &clientError{http.StatusNotFound, "conversation not found"}
	errMessageMissing = &clientError{http.StatusNotFound, "message not found"}
	errNotSender      = &clientError{http.StatusForbidden, "you can only change your own messages"}
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeError answers with a clientError's status and message, or logs
// anything else and answers with a generic 500
func writeError(w http.ResponseWriter, err error) {
	var ce *clientError
	if errors.As(err, &ce) {
		writeJSONError(w, ce.msg, ce.code)
		return
	}
	log.Println("❌", err)
	writeJSONError(w, "Server error", http.StatusInternalServerError)
}

// pathID parses the {id} wildcard of the matched route
func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, badRequest("invalid id")
	}
	return id, nil
}
