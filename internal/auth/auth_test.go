package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/example/gitops-dashboard/internal/config"
	"golang.org/x/crypto/bcrypt"
)

func TestBasicAuthMiddleware(t *testing.T) {
	t.Parallel()
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	auth := New(config.AuthConfig{
		Mode: "basic",
		Users: []config.AuthUser{{
			Username:     "admin",
			PasswordHash: string(hash),
		}},
	})
	handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated code = %d, want 401", res.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "secret")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusNoContent {
		t.Fatalf("authenticated code = %d, want 204", res.Code)
	}
}

func TestAgentTokenAuthenticatorBindsTokensToTargets(t *testing.T) {
	t.Parallel()
	auth := NewAgentTokenAuthenticator(config.Config{
		Auth: config.AuthConfig{
			Agent: config.AgentAuthCfg{Tokens: []string{"shared-token"}},
		},
		Runtime: config.RuntimeConfig{
			Docker: []config.DockerTarget{
				{Name: "serenity", Kind: "agent", AgentToken: "serenity-token"},
				{Name: "albert", Kind: "agent"},
				{Name: "local", Kind: "socket", AgentToken: "ignored-token"},
			},
		},
	})

	binding, ok := auth.Authenticate("shared-token")
	if !ok {
		t.Fatal("shared token did not authenticate")
	}
	if !binding.Allows("serenity") || !binding.Allows("albert") {
		t.Fatalf("shared token targets = %#v, want serenity and albert", binding.Targets())
	}
	if binding.Allows("local") {
		t.Fatalf("shared token targets = %#v, want non-agent target excluded", binding.Targets())
	}

	binding, ok = auth.Authenticate("serenity-token")
	if !ok {
		t.Fatal("target token did not authenticate")
	}
	if !binding.Allows("serenity") || binding.Allows("albert") {
		t.Fatalf("target token targets = %#v, want only serenity", binding.Targets())
	}

	if _, ok := auth.Authenticate("ignored-token"); ok {
		t.Fatal("non-agent target token authenticated")
	}
}

func TestConstantTimeStringEqualUsesSubtleHashComparison(t *testing.T) {
	t.Parallel()
	if constantTimeStringEqual("same-token", "same-token") != 1 {
		t.Fatal("equal tokens did not match")
	}
	if constantTimeStringEqual("same-token", "other-token") != 0 {
		t.Fatal("different tokens matched")
	}
	source, err := os.ReadFile("auth.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "sha256.Sum256") || !strings.Contains(string(source), "subtle.ConstantTimeCompare") {
		t.Fatal("agent token comparison must hash both values and use subtle.ConstantTimeCompare")
	}
}
