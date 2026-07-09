package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
)

func TestNewHTTPServerConfiguresTimeouts(t *testing.T) {
	server := newHTTPServer(config.Config{
		Server: config.ServerConfig{Listen: "127.0.0.1:0"},
	}, http.NewServeMux())

	if server.ReadHeaderTimeout != serverReadHeaderTimeout {
		t.Fatalf("ReadHeaderTimeout = %s, want %s", server.ReadHeaderTimeout, serverReadHeaderTimeout)
	}
	if server.ReadTimeout != serverReadTimeout {
		t.Fatalf("ReadTimeout = %s, want %s", server.ReadTimeout, serverReadTimeout)
	}
	if server.WriteTimeout != serverWriteTimeout {
		t.Fatalf("WriteTimeout = %s, want %s", server.WriteTimeout, serverWriteTimeout)
	}
	if server.IdleTimeout != serverIdleTimeout {
		t.Fatalf("IdleTimeout = %s, want %s", server.IdleTimeout, serverIdleTimeout)
	}
	if server.WriteTimeout < time.Minute {
		t.Fatalf("WriteTimeout = %s, want enough room for websocket ping/pong cadence", server.WriteTimeout)
	}
	if server.Handler == nil {
		t.Fatal("Handler is nil")
	}
}
