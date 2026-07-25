package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/agentprotocol"
	"github.com/example/gitops-dashboard/internal/app"
	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/dockerapi"
	"github.com/gorilla/websocket"
)

// --- shared test scaffolding -----------------------------------------------

func newFakeDockerServer(t *testing.T, containers []dockerapi.Container) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			_ = json.NewEncoder(w).Encode(containers)
		case strings.HasPrefix(r.URL.Path, "/images/"):
			_ = json.NewEncoder(w).Encode(dockerapi.ImageInspect{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func newBlockingDockerServer(t *testing.T, release <-chan struct{}, containers []dockerapi.Container) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			<-release
			_ = json.NewEncoder(w).Encode(containers)
		case strings.HasPrefix(r.URL.Path, "/images/"):
			_ = json.NewEncoder(w).Encode(dockerapi.ImageInspect{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// newFakeAgentServer runs a bare WebSocket upgrader (deliberately not
// internal/app's real handler, which internal/core is frozen against and
// which this package must not import except in the E2E test) so agent-side
// tests can control exactly what bytes come back over the wire.
func newFakeAgentServer(t *testing.T, handle func(t *testing.T, conn *websocket.Conn)) string {
	t.Helper()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		handle(t, conn)
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func validAckJSON(status, code string) string {
	return fmt.Sprintf(`{"type":%q,"status":%q,"code":%q}`, agentprotocol.AckType, status, code)
}

// --- TestSendOnceCollectsAndNormalizesBeforeDial ----------------------------

func TestSendOnceCollectsAndNormalizesBeforeDial(t *testing.T) {
	t.Run("empty_collection_normalizes_to_non_null_containers", func(t *testing.T) {
		t.Parallel()
		dockerHost := newFakeDockerServer(t, nil)
		message, err := collectDocker(context.Background(), config.AgentConfig{Target: "serenity", Docker: config.DockerTarget{Host: dockerHost}})
		if err != nil {
			t.Fatal(err)
		}
		normalizeAgentMessage(&message)
		if message.Containers == nil {
			t.Fatal("containers is null, want a non-null empty slice")
		}
		if len(message.Containers) != 0 {
			t.Fatalf("containers = %#v, want empty", message.Containers)
		}
	})

	// This subtest is deliberately not t.Parallel(): it overrides the
	// package-level agentAckReadTimeout, matching the convention used
	// elsewhere in this task for tests that mutate connection-lifecycle
	// package variables.
	t.Run("slow_collection_proves_collect_before_dial_and_fresh_ack_deadline", func(t *testing.T) {
		oldAckTimeout := agentAckReadTimeout
		agentAckReadTimeout = 150 * time.Millisecond
		defer func() { agentAckReadTimeout = oldAckTimeout }()

		dockerRelease := make(chan struct{})
		dockerHost := newBlockingDockerServer(t, dockerRelease, []dockerapi.Container{
			{ID: "c1", Names: []string{"/svc-1"}, Image: "example/svc:v1", ImageID: "sha256:svc", State: "exited", Status: "Exited (0) 1 hour ago"},
		})

		var handshakes int32
		received := make(chan []byte, 1)
		wsURL := newFakeAgentServer(t, func(t *testing.T, conn *websocket.Conn) {
			atomic.AddInt32(&handshakes, 1)
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			received <- data
			// Responds promptly: well within the shrunk ack deadline
			// measured from the write, but well after collection's
			// artificial delay measured from process start. If the ack
			// deadline were (incorrectly) started before collection/dial,
			// this reply would already be too late.
			_ = conn.WriteMessage(websocket.TextMessage, []byte(validAckJSON(agentprotocol.AckStatusOK, agentprotocol.AckCodePersisted)))
		})

		cfg := config.Config{Agent: config.AgentConfig{
			ServerURL: wsURL,
			Token:     "token",
			Target:    "serenity",
			Docker:    config.DockerTarget{Host: dockerHost},
		}}

		done := make(chan error, 1)
		go func() { done <- sendOnce(context.Background(), cfg) }()

		time.Sleep(200 * time.Millisecond)
		if got := atomic.LoadInt32(&handshakes); got != 0 {
			t.Fatalf("handshakes = %d before releasing collection, want 0", got)
		}

		close(dockerRelease)

		var raw []byte
		select {
		case raw = <-received:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for the report to reach the server")
		}
		if got := atomic.LoadInt32(&handshakes); got != 1 {
			t.Fatalf("handshakes = %d after releasing collection, want exactly 1", got)
		}

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("sendOnce() = %v, want nil (proves the ack deadline started fresh after collection/dial/write, not before)", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for sendOnce to return")
		}

		var message core.AgentMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatal(err)
		}
		if message.Containers == nil {
			t.Fatal("containers is null, want a non-null slice")
		}
		if len(message.Containers) != 1 {
			t.Fatalf("containers = %#v, want exactly one", message.Containers)
		}
		if message.Containers[0].RepoDigests == nil {
			t.Fatal("container repoDigests is null, want a non-null slice")
		}
	})
}

// --- TestSendOnceClassifiesHandshakeRejection -------------------------------

func TestSendOnceClassifiesHandshakeRejection(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			t.Cleanup(server.Close)
			dockerHost := newFakeDockerServer(t, nil)
			cfg := config.Config{Agent: config.AgentConfig{
				ServerURL: "ws" + strings.TrimPrefix(server.URL, "http"),
				Token:     "super-secret-agent-token",
				Target:    "serenity",
				Docker:    config.DockerTarget{Host: dockerHost},
			}}
			err := sendOnce(context.Background(), cfg)
			if !errors.Is(err, errAgentServerRejection) {
				t.Fatalf("sendOnce() = %v, want server_rejection", err)
			}
			if strings.Contains(err.Error(), "super-secret-agent-token") {
				t.Fatalf("error exposed the agent token: %v", err)
			}
		})
	}
}

// --- TestSendOnceClassifiesConnectionFailures -------------------------------

func TestSendOnceClassifiesConnectionFailures(t *testing.T) {
	t.Parallel()

	t.Run("connection_refused", func(t *testing.T) {
		t.Parallel()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := listener.Addr().String()
		_ = listener.Close() // now nothing is listening on addr

		dockerHost := newFakeDockerServer(t, nil)
		cfg := config.Config{Agent: config.AgentConfig{
			ServerURL: "ws://" + addr,
			Token:     "token",
			Target:    "serenity",
			Docker:    config.DockerTarget{Host: dockerHost},
		}}
		err = sendOnce(context.Background(), cfg)
		if !errors.Is(err, errAgentConnectionFailure) {
			t.Fatalf("sendOnce() = %v, want connection_failure", err)
		}
	})

	t.Run("malformed_server_url", func(t *testing.T) {
		t.Parallel()
		dockerHost := newFakeDockerServer(t, nil)
		cfg := config.Config{Agent: config.AgentConfig{
			ServerURL: "ws://%zz",
			Token:     "token",
			Target:    "serenity",
			Docker:    config.DockerTarget{Host: dockerHost},
		}}
		err := sendOnce(context.Background(), cfg)
		if !errors.Is(err, errAgentConnectionFailure) {
			t.Fatalf("sendOnce() = %v, want connection_failure", err)
		}
	})
}

// --- TestSendOnceAcknowledgementMatrix ---------------------------------------

func TestSendOnceAcknowledgementMatrix(t *testing.T) {
	t.Parallel()

	type ackCase struct {
		name      string
		binary    bool
		closeOnly bool
		rawJSON   string
		wantErr   error
	}

	cases := []ackCase{
		{name: "ok_persisted", rawJSON: validAckJSON(agentprotocol.AckStatusOK, agentprotocol.AckCodePersisted), wantErr: nil},
		{name: "error_unauthorized_target", rawJSON: validAckJSON(agentprotocol.AckStatusError, agentprotocol.AckCodeUnauthorizedTarget), wantErr: errAgentServerRejection},
		{name: "error_invalid_report", rawJSON: validAckJSON(agentprotocol.AckStatusError, agentprotocol.AckCodeInvalidReport), wantErr: errAgentServerRejection},
		{name: "error_persistence_failed", rawJSON: validAckJSON(agentprotocol.AckStatusError, agentprotocol.AckCodePersistenceFailed), wantErr: errAgentServerRejection},
		{name: "ok_with_unauthorized_target_code", rawJSON: validAckJSON(agentprotocol.AckStatusOK, agentprotocol.AckCodeUnauthorizedTarget), wantErr: errAgentProtocolFailure},
		{name: "ok_with_invalid_report_code", rawJSON: validAckJSON(agentprotocol.AckStatusOK, agentprotocol.AckCodeInvalidReport), wantErr: errAgentProtocolFailure},
		{name: "ok_with_persistence_failed_code", rawJSON: validAckJSON(agentprotocol.AckStatusOK, agentprotocol.AckCodePersistenceFailed), wantErr: errAgentProtocolFailure},
		{name: "error_with_persisted_code", rawJSON: validAckJSON(agentprotocol.AckStatusError, agentprotocol.AckCodePersisted), wantErr: errAgentProtocolFailure},
		{name: "unknown_status", rawJSON: validAckJSON("pending", agentprotocol.AckCodePersisted), wantErr: errAgentProtocolFailure},
		{name: "unknown_code", rawJSON: validAckJSON(agentprotocol.AckStatusError, "network_partition"), wantErr: errAgentProtocolFailure},
		{name: "missing_fields", rawJSON: fmt.Sprintf(`{"type":%q,"status":%q}`, agentprotocol.AckType, agentprotocol.AckStatusOK), wantErr: errAgentProtocolFailure},
		{name: "unknown_fields", rawJSON: fmt.Sprintf(`{"type":%q,"status":%q,"code":%q,"extra":"x"}`, agentprotocol.AckType, agentprotocol.AckStatusOK, agentprotocol.AckCodePersisted), wantErr: errAgentProtocolFailure},
		{name: "malformed_json", rawJSON: `{not valid json`, wantErr: errAgentProtocolFailure},
		{name: "binary_acknowledgement", binary: true, rawJSON: validAckJSON(agentprotocol.AckStatusOK, agentprotocol.AckCodePersisted), wantErr: errAgentProtocolFailure},
		{name: "early_close", closeOnly: true, wantErr: errAgentProtocolFailure},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dockerHost := newFakeDockerServer(t, nil)
			wsURL := newFakeAgentServer(t, func(t *testing.T, conn *websocket.Conn) {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				if tc.closeOnly {
					return
				}
				messageType := websocket.TextMessage
				if tc.binary {
					messageType = websocket.BinaryMessage
				}
				_ = conn.WriteMessage(messageType, []byte(tc.rawJSON))
			})
			cfg := config.Config{Agent: config.AgentConfig{
				ServerURL: wsURL,
				Token:     "token",
				Target:    "serenity",
				Docker:    config.DockerTarget{Host: dockerHost},
			}}
			err := sendOnce(context.Background(), cfg)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("sendOnce() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("sendOnce() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// --- TestSendOnceRejectsOversizedAcknowledgement -----------------------------

func TestSendOnceRejectsOversizedAcknowledgement(t *testing.T) {
	t.Parallel()
	dockerHost := newFakeDockerServer(t, nil)
	wsURL := newFakeAgentServer(t, func(t *testing.T, conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		oversized := `{"type":"agent_report_ack","status":"ok","code":"` + strings.Repeat("x", 8192) + `"}`
		_ = conn.WriteMessage(websocket.TextMessage, []byte(oversized))
	})
	cfg := config.Config{Agent: config.AgentConfig{
		ServerURL: wsURL,
		Token:     "token",
		Target:    "serenity",
		Docker:    config.DockerTarget{Host: dockerHost},
	}}
	err := sendOnce(context.Background(), cfg)
	if !errors.Is(err, errAgentProtocolFailure) {
		t.Fatalf("sendOnce() = %v, want protocol_failure", err)
	}
}

// --- TestAgentReportAcknowledgementE2E ---------------------------------------

// TestAgentReportAcknowledgementE2E exercises the real protocol end to end:
// production sendOnce against the real internal/app server handler and real
// persistence, then confirms the report is queryable through /api/summary.
// It lives in this package (not agent_test) specifically so it can call the
// private sendOnce directly, per the task spec.
func TestAgentReportAcknowledgementE2E(t *testing.T) {
	t.Parallel()

	dockerHost := newFakeDockerServer(t, []dockerapi.Container{
		{ID: "c1", Names: []string{"/stack-web-1"}, Image: "example/web:v1", ImageID: "sha256:web", State: "running", Status: "Up 1 minute"},
	})

	serverCfg := config.Config{
		Server: config.ServerConfig{
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth: config.AuthConfig{
			Mode:  "dev-no-auth",
			Agent: config.AgentAuthCfg{Tokens: []string{"agent-token"}},
		},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
		Runtime: config.RuntimeConfig{
			Docker: []config.DockerTarget{{Name: "serenity", Kind: "agent"}},
		},
	}
	application, err := app.New(serverCfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(application.Close)
	server := httptest.NewServer(application.Handler())
	t.Cleanup(server.Close)

	agentCfg := config.Config{Agent: config.AgentConfig{
		ServerURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/api/agents/connect",
		Token:     "agent-token",
		Target:    "serenity",
		Docker:    config.DockerTarget{Host: dockerHost},
	}}

	if err := sendOnce(context.Background(), agentCfg); err != nil {
		t.Fatalf("sendOnce() = %v, want nil", err)
	}

	res := httptest.NewRecorder()
	application.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("summary status = %d", res.Code)
	}
	var summary core.DashboardSummary
	if err := json.Unmarshal(res.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	var serenity *core.AgentInfo
	for i := range summary.Agents {
		if summary.Agents[i].Target == "serenity" {
			serenity = &summary.Agents[i]
		}
	}
	if serenity == nil || serenity.LastSeenAt == "" {
		t.Fatalf("agents = %#v, want serenity already persisted since sendOnce only returns success after receipt", summary.Agents)
	}
	if len(serenity.Containers) != 1 || serenity.Containers[0].Name != "/stack-web-1" {
		t.Fatalf("containers = %#v", serenity.Containers)
	}
}
