package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var jwtKey = []byte("super-secret-key") // ⚠️ move to env in real app

type Claims struct {
	Username string `json:"username"`
	jwt.RegisteredClaims
}

func generateJWT(username string) (string, error) {
	expiration := time.Now().Add(24 * time.Hour)
	claims := &Claims{
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiration),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtKey)
}

func validateJWT(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(token *jwt.Token) (interface{}, error) {
		return jwtKey, nil
	})
	if err != nil || !token.Valid {
		return nil, err
	}
	return claims, nil
}

// authUser returns the username in the request's JWT. The token comes from an
// "Authorization: Bearer" header, or from ?token= where a browser can't set
// headers (WebSocket connections and <img src> attachment links).
func authUser(r *http.Request) (string, bool) {
	tokenStr := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tokenStr == "" {
		tokenStr = r.URL.Query().Get("token")
	}
	claims, err := validateJWT(tokenStr)
	if err != nil || claims == nil {
		return "", false
	}
	return claims.Username, true
}

// withAuth rejects requests without a valid token and passes the username on
func withAuth(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := authUser(r)
		if !ok {
			writeJSONError(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r, user)
	}
}
