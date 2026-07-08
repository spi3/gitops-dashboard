package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"sort"
	"strings"

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
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func isPublicPath(path string) bool {
	return path == "/healthz" || path == "/readyz" || path == "/api/agents/connect"
}

type AgentTokenAuthenticator struct {
	records []agentTokenRecord
}

type AgentTokenBinding struct {
	targets map[string]struct{}
}

type agentTokenRecord struct {
	token   string
	targets []string
}

func NewAgentTokenAuthenticator(cfg config.Config) AgentTokenAuthenticator {
	targets := configuredAgentTargets(cfg.Runtime.Docker)
	records := make([]agentTokenRecord, 0, len(cfg.Auth.Agent.Tokens)+len(cfg.Runtime.Docker))
	for _, token := range cfg.Auth.Agent.Tokens {
		token = strings.TrimSpace(token)
		if token == "" || len(targets) == 0 {
			continue
		}
		records = append(records, agentTokenRecord{
			token:   token,
			targets: append([]string(nil), targets...),
		})
	}
	for _, target := range cfg.Runtime.Docker {
		if target.Kind != "agent" {
			continue
		}
		name := strings.TrimSpace(target.Name)
		token := strings.TrimSpace(target.AgentToken)
		if name == "" || token == "" {
			continue
		}
		records = append(records, agentTokenRecord{
			token:   token,
			targets: []string{name},
		})
	}
	return AgentTokenAuthenticator{records: records}
}

func (auth AgentTokenAuthenticator) Authenticate(token string) (AgentTokenBinding, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return AgentTokenBinding{}, false
	}
	matched := 0
	targets := map[string]struct{}{}
	for _, record := range auth.records {
		equal := constantTimeStringEqual(token, record.token)
		matched |= equal
		if equal == 1 {
			for _, target := range record.targets {
				targets[target] = struct{}{}
			}
		}
	}
	if matched != 1 || len(targets) == 0 {
		return AgentTokenBinding{}, false
	}
	return AgentTokenBinding{targets: targets}, true
}

func (binding AgentTokenBinding) Allows(target string) bool {
	_, ok := binding.targets[strings.TrimSpace(target)]
	return ok
}

func (binding AgentTokenBinding) Targets() []string {
	targets := make([]string, 0, len(binding.targets))
	for target := range binding.targets {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return targets
}

func configuredAgentTargets(targets []config.DockerTarget) []string {
	names := make([]string, 0, len(targets))
	seen := map[string]struct{}{}
	for _, target := range targets {
		if target.Kind != "agent" {
			continue
		}
		name := strings.TrimSpace(target.Name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func constantTimeStringEqual(candidate, configured string) int {
	candidateHash := sha256.Sum256([]byte(candidate))
	configuredHash := sha256.Sum256([]byte(configured))
	return subtle.ConstantTimeCompare(candidateHash[:], configuredHash[:])
}
