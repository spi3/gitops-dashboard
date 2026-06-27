package auth

import (
	"crypto/subtle"
	"net/http"

	"github.com/example/gitops-dashboard/internal/config"
	"golang.org/x/crypto/bcrypt"
)

type BasicAuth struct {
	mode  string
	users map[string]string
}

func New(cfg config.AuthConfig) BasicAuth {
	users := make(map[string]string, len(cfg.Users))
	for _, user := range cfg.Users {
		users[user.Username] = user.PasswordHash
	}
	return BasicAuth{mode: cfg.Mode, users: users}
}

func (auth BasicAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.mode == "dev-no-auth" || isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		username, password, ok := r.BasicAuth()
		if !ok || !auth.valid(username, password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="gitops-dashboard"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (auth BasicAuth) valid(username, password string) bool {
	hash, ok := auth.users[username]
	if !ok {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(username), []byte(username)) != 1 {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func isPublicPath(path string) bool {
	return path == "/healthz" || path == "/readyz" || path == "/api/agents/connect"
}
