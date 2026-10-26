package main

import (
	"net/http"
)

type userStatus struct {
	Username string `json:"username"`
	Online   bool   `json:"online"`
}

func usersHandler(w http.ResponseWriter, r *http.Request, user string) {
	rows, err := db.Query("SELECT username FROM users ORDER BY username")
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()

	users := []userStatus{}
	for rows.Next() {
		var username string
		if err := rows.Scan(&username); err == nil {
			users = append(users, userStatus{Username: username, Online: chatHub.isOnline(username)})
		}
	}

	writeJSON(w, http.StatusOK, map[string][]userStatus{
		"users": users,
	})
}
