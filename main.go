package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	initDB()

	// Pick port from environment variable or default to 8080
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/register", registerHandler)
	mux.HandleFunc("/login", loginHandler)
	mux.HandleFunc("/ws", handleWS)
	mux.HandleFunc("GET /users", withAuth(usersHandler))
	mux.HandleFunc("GET /conversations", withAuth(conversationsHandler))
	mux.HandleFunc("POST /conversations", withAuth(createConversationHandler))
	mux.HandleFunc("GET /conversations/{id}/messages", withAuth(messagesHandler))
	mux.HandleFunc("DELETE /conversations/{id}/messages", withAuth(clearHistoryHandler))
	mux.HandleFunc("POST /conversations/{id}/members", withAuth(addMembersHandler))
	mux.HandleFunc("POST /conversations/{id}/leave", withAuth(leaveConversationHandler))
	mux.HandleFunc("POST /upload", withAuth(uploadHandler))
	mux.HandleFunc("GET /attachments/{id}", withAuth(attachmentHandler))

	// Serve static frontend
	mux.Handle("/", http.FileServer(http.Dir("./static")))

	log.Println("Server running on :" + port)
	log.Fatal(http.ListenAndServe(":"+port, enableCORS(mux)))
}

// CORS Middleware
func enableCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		// Handle preflight (OPTIONS) request
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
