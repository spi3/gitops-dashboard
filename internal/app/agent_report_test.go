package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/agentprotocol"
	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/gorilla/websocket"
)

// --- shared test scaffolding -----------------------------------------------

func newAgentReportTestApp(t *testing.T, targets ...string) (*App, string) {
	t.Helper()
	return newAgentReportTestAppWithLogger(t, slog.Default(), targets...)
}

func newAgentReportTestAppWithLogger(t *testing.T, logger *slog.Logger, targets ...string) (*App, string) {
	t.Helper()
	if len(targets) == 0 {
		targets = []string{"serenity"}
	}
	dockerTargets := make([]config.DockerTarget, len(targets))
	for i, name := range targets {
		dockerTargets[i] = config.DockerTarget{Name: name, Kind: "agent"}
	}
	cfg := config.Config{
		Server: config.ServerConfig{
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth: config.AuthConfig{
			Mode:  "dev-no-auth",
			Agent: config.AgentAuthCfg{Tokens: []string{"valid-token"}},
		},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
		Runtime:    config.RuntimeConfig{Docker: dockerTargets},
	}
	app, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)
	// httptest.Server.Close does not wait for hijacked (WebSocket)
	// connections, so wait for every agentConnect call — including its
	// keepalive ping goroutine — to actually finish before the next test
	// might mutate a package-level deadline/limit variable this one used.
	t.Cleanup(app.agentConnsWG.Wait)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/agents/connect"
	return app, wsURL
}

func dialAgentReportConn(t *testing.T, wsURL, token string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{agentTokenHeader: []string{token}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func validAgentReportContainer(id string) map[string]any {
	return map[string]any{
		"id":           id,
		"name":         "/stack-web-1",
		"image":        "example/web:v1",
		"imageId":      "sha256:web",
		"repoDigests":  []string{"example/web@sha256:release"},
		"state":        "running",
		"status":       "Up 1 minute",
		"health":       "healthy",
		"restartCount": 0,
	}
}

// minimalAgentReportContainer holds only the schema's required fields at
// their smallest valid size, for tests that need many containers in one
// message while staying well under the WebSocket read limit.
func minimalAgentReportContainer(id string) map[string]any {
	return map[string]any{
		"id":           id,
		"name":         "",
		"image":        "",
		"imageId":      "",
		"repoDigests":  []string{},
		"state":        "",
		"status":       "",
		"health":       "none",
		"restartCount": 0,
	}
}

func validAgentReport(target string, containers ...map[string]any) map[string]any {
	items := make([]any, len(containers))
	for i, container := range containers {
		items[i] = container
	}
	return map[string]any{
		"target":     target,
		"checkedAt":  time.Now().UTC().Format(time.RFC3339),
		"containers": items,
	}
}

func marshalAgentReport(t *testing.T, report map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readAgentReportAck(t *testing.T, conn *websocket.Conn) agentReportAck {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var ack agentReportAck
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("read acknowledgement: %v", err)
	}
	return ack
}

func readAgentReportCloseCode(t *testing.T, conn *websocket.Conn) int {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := conn.ReadMessage()
	closeErr, ok := err.(*websocket.CloseError)
	if !ok {
		t.Fatalf("read error = %v, want a close error", err)
	}
	return closeErr.Code
}

// assertAgentReportRejected sends data over a fresh connection and asserts
// the exact typed error acknowledgement and close code the protocol pins to
// a given failure category.
func assertAgentReportRejected(t *testing.T, wsURL string, data []byte, wantCode string, wantClose int) {
	t.Helper()
	conn := dialAgentReportConn(t, wsURL, "valid-token")
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
	ack := readAgentReportAck(t, conn)
	if ack.Type != agentprotocol.AckType || ack.Status != agentprotocol.AckStatusError || ack.Code != wantCode {
		t.Fatalf("ack = %#v, want error/%s", ack, wantCode)
	}
	if code := readAgentReportCloseCode(t, conn); code != wantClose {
		t.Fatalf("close code = %d, want %d", code, wantClose)
	}
}

func assertAgentReportSchemaInvalid(t *testing.T, wsURL string, data []byte) {
	t.Helper()
	assertAgentReportRejected(t, wsURL, data, agentprotocol.AckCodeInvalidReport, websocket.CloseInvalidFramePayloadData)
}

func assertAgentReportSemanticInvalid(t *testing.T, wsURL string, data []byte) {
	t.Helper()
	assertAgentReportRejected(t, wsURL, data, agentprotocol.AckCodeInvalidReport, websocket.ClosePolicyViolation)
}

func assertAgentReportPersisted(t *testing.T, wsURL string, data []byte) {
	t.Helper()
	conn := dialAgentReportConn(t, wsURL, "valid-token")
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
	ack := readAgentReportAck(t, conn)
	if ack.Type != agentprotocol.AckType || ack.Status != agentprotocol.AckStatusOK || ack.Code != agentprotocol.AckCodePersisted {
		t.Fatalf("ack = %#v, want ok/persisted", ack)
	}
	if code := readAgentReportCloseCode(t, conn); code != websocket.CloseNormalClosure {
		t.Fatalf("close code = %d, want %d", code, websocket.CloseNormalClosure)
	}
}

func agentReportDigestList(n int) []string {
	digests := make([]string, n)
	for i := range digests {
		digests[i] = fmt.Sprintf("example/web@sha256:%064d", i)
	}
	return digests
}

func agentReportLabelMap(n int) map[string]string {
	labels := make(map[string]string, n)
	for i := 0; i < n; i++ {
		labels[fmt.Sprintf("label-%d", i)] = "value"
	}
	return labels
}

func agentReportsContainTarget(agents []core.AgentInfo, target string) bool {
	for _, agent := range agents {
		if agent.Target == target {
			return true
		}
	}
	return false
}

// --- TestAgentReportRejectsBinaryMessage ------------------------------------

func TestAgentReportRejectsBinaryMessage(t *testing.T) {
	t.Parallel()
	_, wsURL := newAgentReportTestApp(t)
	conn := dialAgentReportConn(t, wsURL, "valid-token")

	report := validAgentReport("serenity", validAgentReportContainer("container-1"))
	if err := conn.WriteMessage(websocket.BinaryMessage, marshalAgentReport(t, report)); err != nil {
		t.Fatal(err)
	}
	ack := readAgentReportAck(t, conn)
	if ack.Type != agentprotocol.AckType || ack.Status != agentprotocol.AckStatusError || ack.Code != agentprotocol.AckCodeInvalidReport {
		t.Fatalf("ack = %#v, want error/invalid_report", ack)
	}
	if code := readAgentReportCloseCode(t, conn); code != websocket.CloseInvalidFramePayloadData {
		t.Fatalf("close code = %d, want %d", code, websocket.CloseInvalidFramePayloadData)
	}
}

// --- TestAgentReportWireSchemaMissingAndNullFields --------------------------

func TestAgentReportWireSchemaMissingAndNullFields(t *testing.T) {
	t.Parallel()
	_, wsURL := newAgentReportTestApp(t)

	topFields := []string{"target", "checkedAt", "containers"}
	containerFields := []string{"id", "name", "image", "imageId", "repoDigests", "state", "status", "health", "restartCount"}

	for _, field := range topFields {
		field := field
		t.Run("missing_top_level_"+field, func(t *testing.T) {
			t.Parallel()
			report := validAgentReport("serenity", validAgentReportContainer("container-1"))
			delete(report, field)
			assertAgentReportSchemaInvalid(t, wsURL, marshalAgentReport(t, report))
		})
		t.Run("null_top_level_"+field, func(t *testing.T) {
			t.Parallel()
			report := validAgentReport("serenity", validAgentReportContainer("container-1"))
			report[field] = nil
			assertAgentReportSchemaInvalid(t, wsURL, marshalAgentReport(t, report))
		})
	}

	for _, field := range containerFields {
		field := field
		t.Run("missing_container_"+field, func(t *testing.T) {
			t.Parallel()
			container := validAgentReportContainer("container-1")
			delete(container, field)
			report := validAgentReport("serenity", container)
			assertAgentReportSchemaInvalid(t, wsURL, marshalAgentReport(t, report))
		})
		t.Run("null_container_"+field, func(t *testing.T) {
			t.Parallel()
			container := validAgentReportContainer("container-1")
			container[field] = nil
			report := validAgentReport("serenity", container)
			assertAgentReportSchemaInvalid(t, wsURL, marshalAgentReport(t, report))
		})
	}

	t.Run("missing_labels_is_valid", func(t *testing.T) {
		t.Parallel()
		container := validAgentReportContainer("container-1")
		delete(container, "labels")
		report := validAgentReport("serenity", container)
		assertAgentReportPersisted(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("null_labels_is_invalid", func(t *testing.T) {
		t.Parallel()
		container := validAgentReportContainer("container-1")
		container["labels"] = nil
		report := validAgentReport("serenity", container)
		assertAgentReportSchemaInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("null_container_entry", func(t *testing.T) {
		t.Parallel()
		report := validAgentReport("serenity")
		report["containers"] = []any{nil}
		assertAgentReportSchemaInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("unknown_container_field", func(t *testing.T) {
		t.Parallel()
		container := validAgentReportContainer("container-1")
		container["unexpected"] = "value"
		report := validAgentReport("serenity", container)
		assertAgentReportSchemaInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("non_string_label_values", func(t *testing.T) {
		t.Parallel()
		container := validAgentReportContainer("container-1")
		container["labels"] = map[string]any{core.DockerComposeProjectLabel: 5}
		report := validAgentReport("serenity", container)
		assertAgentReportSchemaInvalid(t, wsURL, marshalAgentReport(t, report))
	})
}

// --- TestAgentReportWireSchemaBoundaries ------------------------------------

// TestAgentReportWireSchemaBoundaries does not run in parallel at the top
// level: its containers_10000/10001 subtests temporarily raise the
// package-level agentWSReadLimit (10,000 required-field-only containers do
// not fit under the production 1 MiB limit) and must not race any other test
// that dials against the default limit, matching the convention already
// used by TestAgentWebSocketReadLimitClosesOversizedMessage.
func TestAgentReportWireSchemaBoundaries(t *testing.T) {
	longTarget := strings.Repeat("a", 255)
	_, wsURL := newAgentReportTestApp(t, "serenity", longTarget)

	t.Run("target_255_bytes_accepted", func(t *testing.T) {
		t.Parallel()
		report := validAgentReport(longTarget, validAgentReportContainer("container-1"))
		assertAgentReportPersisted(t, wsURL, marshalAgentReport(t, report))
	})
	t.Run("target_256_bytes_rejected", func(t *testing.T) {
		t.Parallel()
		report := validAgentReport(strings.Repeat("a", 256), validAgentReportContainer("container-1"))
		assertAgentReportSemanticInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("id_255_bytes_accepted", func(t *testing.T) {
		t.Parallel()
		report := validAgentReport("serenity", validAgentReportContainer(strings.Repeat("a", 255)))
		assertAgentReportPersisted(t, wsURL, marshalAgentReport(t, report))
	})
	t.Run("id_256_bytes_accepted", func(t *testing.T) {
		t.Parallel()
		report := validAgentReport("serenity", validAgentReportContainer(strings.Repeat("a", 256)))
		assertAgentReportPersisted(t, wsURL, marshalAgentReport(t, report))
	})
	t.Run("id_257_bytes_rejected", func(t *testing.T) {
		t.Parallel()
		report := validAgentReport("serenity", validAgentReportContainer(strings.Repeat("a", 257)))
		assertAgentReportSemanticInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	// 10,000 containers, even with only their required fields present at
	// minimum size, do not fit under the production 1 MiB WebSocket read
	// limit; these two subtests raise it locally (and are deliberately not
	// t.Parallel(), see the containing test's doc comment) so the container
	// *count* bound is what gets exercised here, not the read limit.
	t.Run("containers_10000_accepted", func(t *testing.T) {
		oldLimit := agentWSReadLimit
		agentWSReadLimit = 4 << 20
		defer func() { agentWSReadLimit = oldLimit }()

		containers := make([]map[string]any, 10000)
		for i := range containers {
			containers[i] = minimalAgentReportContainer(fmt.Sprintf("%d", i))
		}
		report := validAgentReport("serenity", containers...)
		assertAgentReportPersisted(t, wsURL, marshalAgentReport(t, report))
	})
	t.Run("containers_10001_rejected", func(t *testing.T) {
		oldLimit := agentWSReadLimit
		agentWSReadLimit = 4 << 20
		defer func() { agentWSReadLimit = oldLimit }()

		containers := make([]map[string]any, 10001)
		for i := range containers {
			containers[i] = minimalAgentReportContainer(fmt.Sprintf("%d", i))
		}
		report := validAgentReport("serenity", containers...)
		assertAgentReportSemanticInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("duplicate_container_ids_rejected", func(t *testing.T) {
		t.Parallel()
		report := validAgentReport("serenity", validAgentReportContainer("dup"), validAgentReportContainer("dup"))
		assertAgentReportSemanticInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	// health is excluded here: its four valid values are all far shorter
	// than the 4,096-byte bound, so a byte-boundary case for it cannot also
	// stay a valid enum member. It shares the identical length-check code
	// path with the other five scalars (exercised below) and has its own
	// dedicated enum-validity coverage in TestAgentReportResponseMatrix.
	for _, field := range []string{"name", "image", "imageId", "state", "status"} {
		field := field
		t.Run("scalar_"+field+"_4096_bytes_accepted", func(t *testing.T) {
			t.Parallel()
			container := validAgentReportContainer("container-1")
			container[field] = strings.Repeat("a", 4096)
			report := validAgentReport("serenity", container)
			assertAgentReportPersisted(t, wsURL, marshalAgentReport(t, report))
		})
		t.Run("scalar_"+field+"_4097_bytes_rejected", func(t *testing.T) {
			t.Parallel()
			container := validAgentReportContainer("container-1")
			container[field] = strings.Repeat("a", 4097)
			report := validAgentReport("serenity", container)
			assertAgentReportSemanticInvalid(t, wsURL, marshalAgentReport(t, report))
		})
	}

	t.Run("digests_256_accepted", func(t *testing.T) {
		t.Parallel()
		container := validAgentReportContainer("container-1")
		container["repoDigests"] = agentReportDigestList(256)
		report := validAgentReport("serenity", container)
		assertAgentReportPersisted(t, wsURL, marshalAgentReport(t, report))
	})
	t.Run("digests_257_rejected", func(t *testing.T) {
		t.Parallel()
		container := validAgentReportContainer("container-1")
		container["repoDigests"] = agentReportDigestList(257)
		report := validAgentReport("serenity", container)
		assertAgentReportSemanticInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("labels_32_accepted", func(t *testing.T) {
		t.Parallel()
		container := validAgentReportContainer("container-1")
		container["labels"] = agentReportLabelMap(32)
		report := validAgentReport("serenity", container)
		assertAgentReportPersisted(t, wsURL, marshalAgentReport(t, report))
	})
	t.Run("labels_33_rejected", func(t *testing.T) {
		t.Parallel()
		container := validAgentReportContainer("container-1")
		container["labels"] = agentReportLabelMap(33)
		report := validAgentReport("serenity", container)
		assertAgentReportSemanticInvalid(t, wsURL, marshalAgentReport(t, report))
	})
}

// --- TestAgentReportResponseMatrix -------------------------------------------

// TestAgentReportResponseMatrix does not run in parallel, and neither do its
// subtests: several mutate package-level deadline/limit variables that the
// production handler reads on every connection, matching the convention
// already used by TestAgentWebSocketReadLimitClosesOversizedMessage and
// TestAgentWebSocketReadDeadlineClosesIdleConnection.
func TestAgentReportResponseMatrix(t *testing.T) {
	t.Run("handshake_auth_rejected", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		httpURL := "http" + strings.TrimPrefix(wsURL, "ws")
		req, err := http.NewRequest(http.MethodGet, httpURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(agentTokenHeader, "wrong-token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", res.StatusCode)
		}
	})

	t.Run("authorization_rejected", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t, "serenity")
		report := validAgentReport("some-other-target", validAgentReportContainer("container-1"))
		assertAgentReportRejected(t, wsURL, marshalAgentReport(t, report), agentprotocol.AckCodeUnauthorizedTarget, websocket.ClosePolicyViolation)
	})

	t.Run("syntax_invalid_json", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		assertAgentReportSchemaInvalid(t, wsURL, []byte(`{not valid json`))
	})

	t.Run("type_wrong_top_level_value", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		assertAgentReportSchemaInvalid(t, wsURL, []byte(`"just a string"`))
	})

	t.Run("unknown_top_level_field", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		report := validAgentReport("serenity", validAgentReportContainer("container-1"))
		report["extra"] = "unexpected"
		assertAgentReportSchemaInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("trailing_json_after_report", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		report := validAgentReport("serenity", validAgentReportContainer("container-1"))
		data := append(marshalAgentReport(t, report), []byte(" {}")...)
		assertAgentReportSchemaInvalid(t, wsURL, data)
	})

	t.Run("semantic_target_empty_after_trim", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		report := validAgentReport("   ", validAgentReportContainer("container-1"))
		assertAgentReportSemanticInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("semantic_checked_at_not_rfc3339", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		report := validAgentReport("serenity", validAgentReportContainer("container-1"))
		report["checkedAt"] = "not-a-timestamp"
		assertAgentReportSemanticInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("semantic_checked_at_zero", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		report := validAgentReport("serenity", validAgentReportContainer("container-1"))
		report["checkedAt"] = "0001-01-01T00:00:00Z"
		assertAgentReportSemanticInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("semantic_restart_count_negative", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		container := validAgentReportContainer("container-1")
		container["restartCount"] = -1
		report := validAgentReport("serenity", container)
		assertAgentReportSemanticInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("semantic_health_invalid_value", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		container := validAgentReportContainer("container-1")
		container["health"] = "bogus"
		report := validAgentReport("serenity", container)
		assertAgentReportSemanticInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("semantic_container_id_whitespace_only", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		report := validAgentReport("serenity", validAgentReportContainer("   "))
		assertAgentReportSemanticInvalid(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("persistence_failure", func(t *testing.T) {
		app, wsURL := newAgentReportTestApp(t)
		app.applyAgentReport = func(context.Context, core.AgentMessage, []string) error {
			return fmt.Errorf("simulated backing store outage")
		}
		report := validAgentReport("serenity", validAgentReportContainer("container-1"))
		assertAgentReportRejected(t, wsURL, marshalAgentReport(t, report), agentprotocol.AckCodePersistenceFailed, websocket.CloseInternalServerErr)
	})

	t.Run("oversized_message", func(t *testing.T) {
		oldLimit := agentWSReadLimit
		agentWSReadLimit = 64
		defer func() { agentWSReadLimit = oldLimit }()

		app, wsURL := newAgentReportTestApp(t)
		conn := dialAgentReportConn(t, wsURL, "valid-token")
		oversized := append([]byte(`{"target":"serenity","containers":"`), bytes.Repeat([]byte("x"), 128)...)
		if err := conn.WriteMessage(websocket.TextMessage, oversized); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, err := conn.ReadMessage()
		if err == nil {
			t.Fatal("oversized message left websocket open")
		}
		closeErr, ok := err.(*websocket.CloseError)
		if !ok {
			t.Fatalf("read error = %v, want a close error", err)
		}
		if closeErr.Code != websocket.CloseMessageTooBig {
			t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.CloseMessageTooBig)
		}
		// Wait for this connection's handler (and its ping goroutine) to
		// actually finish before the deferred restore above runs: they
		// still read the just-restored-to-default agentWSReadLimit
		// otherwise, which the next subtest could race with.
		app.agentConnsWG.Wait()
	})

	t.Run("broken_protocol", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		conn := dialAgentReportConn(t, wsURL, "valid-token")
		// An unmasked client text frame is a protocol violation: RFC 6455
		// requires every client-to-server frame to be masked. Writing raw
		// bytes to the underlying connection bypasses gorilla's own
		// (correctly masking) write path to construct this directly.
		frame := []byte{0x81, 0x05, 'h', 'e', 'l', 'l', 'o'}
		if _, err := conn.UnderlyingConn().Write(frame); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, err := conn.ReadMessage()
		if err == nil {
			t.Fatal("broken protocol frame left websocket open")
		}
		closeErr, ok := err.(*websocket.CloseError)
		if !ok {
			t.Fatalf("read error = %v, want a close error", err)
		}
		if closeErr.Code != websocket.CloseProtocolError {
			t.Fatalf("close code = %d, want %d", closeErr.Code, websocket.CloseProtocolError)
		}
	})

	t.Run("peer_close_before_report", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		conn := dialAgentReportConn(t, wsURL, "valid-token")
		if err := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		messageType, _, err := conn.ReadMessage()
		if err == nil {
			t.Fatalf("expected no acknowledgement after a peer close, got message type %d", messageType)
		}
		if _, ok := err.(*websocket.CloseError); !ok {
			t.Fatalf("read error = %v, want a close error (no acknowledgement)", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		_, wsURL := newAgentReportTestApp(t)
		report := validAgentReport("serenity", validAgentReportContainer("container-1"))
		assertAgentReportPersisted(t, wsURL, marshalAgentReport(t, report))
	})

	t.Run("refreshed_ack_and_close_deadlines", func(t *testing.T) {
		oldWriteWait := agentWSWriteWait
		agentWSWriteWait = 100 * time.Millisecond
		defer func() { agentWSWriteWait = oldWriteWait }()

		app, wsURL := newAgentReportTestApp(t)
		originalApply := app.applyAgentReport
		app.applyAgentReport = func(ctx context.Context, message core.AgentMessage, authorizedTargets []string) error {
			// Longer than the shrunk write-wait: this proves the ack write
			// deadline is set fresh right before the write, not inherited
			// from an earlier point in the connection's lifetime.
			time.Sleep(300 * time.Millisecond)
			return originalApply(ctx, message, authorizedTargets)
		}

		conn := dialAgentReportConn(t, wsURL, "valid-token")
		report := validAgentReport("serenity", validAgentReportContainer("container-1"))
		if err := conn.WriteMessage(websocket.TextMessage, marshalAgentReport(t, report)); err != nil {
			t.Fatal(err)
		}
		ack := readAgentReportAck(t, conn)
		if ack.Status != agentprotocol.AckStatusOK || ack.Code != agentprotocol.AckCodePersisted {
			t.Fatalf("ack = %#v, want ok/persisted despite a persistence stage slower than the write-wait deadline", ack)
		}
		if code := readAgentReportCloseCode(t, conn); code != websocket.CloseNormalClosure {
			t.Fatalf("close code = %d, want %d", code, websocket.CloseNormalClosure)
		}
		// See the same wait in the oversized_message subtest above: this
		// connection's ping goroutine still reads the shrunk
		// agentWSWriteWait until it actually exits.
		app.agentConnsWG.Wait()
	})

	t.Run("no_sensitive_data_leaks", func(t *testing.T) {
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		app, wsURL := newAgentReportTestAppWithLogger(t, logger, "serenity")

		const tokenSentinel = "SENTINEL-TOKEN-9f4b1c2e"
		const fieldSentinel = "SENTINEL-FIELD-7ad930e1"
		const persistenceSentinel = "SENTINEL-PERSISTENCE-c81ff56a"

		// 1. A rejected handshake token never appears anywhere observable.
		httpURL := "http" + strings.TrimPrefix(wsURL, "ws")
		req, err := http.NewRequest(http.MethodGet, httpURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(agentTokenHeader, tokenSentinel)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", res.StatusCode)
		}
		if strings.Contains(body.String(), tokenSentinel) {
			t.Fatal("rejected handshake token leaked into the HTTP response body")
		}

		// 2. A rejected report's attacker-controlled field name never
		// appears in the acknowledgement, the close reason, or the logs.
		container := validAgentReportContainer("container-1")
		container[fieldSentinel] = "value"
		report := validAgentReport("serenity", container)
		conn := dialAgentReportConn(t, wsURL, "valid-token")
		if err := conn.WriteMessage(websocket.TextMessage, marshalAgentReport(t, report)); err != nil {
			t.Fatal(err)
		}
		ack := readAgentReportAck(t, conn)
		ackBytes, err := json.Marshal(ack)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(ackBytes), fieldSentinel) {
			t.Fatal("rejected report field name leaked into the acknowledgement")
		}
		_, closeReason, err := readAgentReportCloseFrame(t, conn)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(closeReason, fieldSentinel) {
			t.Fatal("rejected report field name leaked into the close reason")
		}

		// 3. An injected persistence error never appears in the
		// acknowledgement, the close reason, or the logs.
		app.applyAgentReport = func(context.Context, core.AgentMessage, []string) error {
			return fmt.Errorf("backing store unavailable: %s", persistenceSentinel)
		}
		validReport := validAgentReport("serenity", validAgentReportContainer("container-2"))
		conn2 := dialAgentReportConn(t, wsURL, "valid-token")
		if err := conn2.WriteMessage(websocket.TextMessage, marshalAgentReport(t, validReport)); err != nil {
			t.Fatal(err)
		}
		ack2 := readAgentReportAck(t, conn2)
		if ack2.Status != agentprotocol.AckStatusError || ack2.Code != agentprotocol.AckCodePersistenceFailed {
			t.Fatalf("ack = %#v, want error/persistence_failed", ack2)
		}
		ack2Bytes, err := json.Marshal(ack2)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(ack2Bytes), persistenceSentinel) {
			t.Fatal("injected persistence error leaked into the acknowledgement")
		}
		_, closeReason2, err := readAgentReportCloseFrame(t, conn2)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(closeReason2, persistenceSentinel) {
			t.Fatal("injected persistence error leaked into the close reason")
		}

		logText := logs.String()
		for _, sentinel := range []string{tokenSentinel, fieldSentinel, persistenceSentinel} {
			if strings.Contains(logText, sentinel) {
				t.Fatalf("sentinel %q leaked into logs:\n%s", sentinel, logText)
			}
		}
	})
}

func readAgentReportCloseFrame(t *testing.T, conn *websocket.Conn) (int, string, error) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := conn.ReadMessage()
	closeErr, ok := err.(*websocket.CloseError)
	if !ok {
		return 0, "", fmt.Errorf("read error = %v, want a close error", err)
	}
	return closeErr.Code, closeErr.Text, nil
}

// --- TestAgentReportPersistsBeforeAcknowledgement ---------------------------

func TestAgentReportPersistsBeforeAcknowledgement(t *testing.T) {
	t.Parallel()
	app, wsURL := newAgentReportTestApp(t)

	persisted := make(chan struct{})
	releaseAck := make(chan struct{})
	originalApply := app.applyAgentReport
	app.applyAgentReport = func(ctx context.Context, message core.AgentMessage, authorizedTargets []string) error {
		if err := originalApply(ctx, message, authorizedTargets); err != nil {
			return err
		}
		close(persisted)
		<-releaseAck
		return nil
	}

	conn := dialAgentReportConn(t, wsURL, "valid-token")
	report := validAgentReport("serenity", validAgentReportContainer("container-1"))
	if err := conn.WriteMessage(websocket.TextMessage, marshalAgentReport(t, report)); err != nil {
		t.Fatal(err)
	}

	ackReceived := make(chan agentReportAck, 1)
	go func() {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var ack agentReportAck
		if err := conn.ReadJSON(&ack); err == nil {
			ackReceived <- ack
		}
	}()

	select {
	case <-persisted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for persistence to complete")
	}

	// The store must already show the persisted report at this point:
	// applyAgentReport cannot even return, let alone let the handler write
	// the acknowledgement, until releaseAck is closed below.
	agents, err := app.store.Agents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !agentReportsContainTarget(agents, "serenity") {
		t.Fatalf("agents = %#v, want serenity already persisted before acknowledgement", agents)
	}

	select {
	case ack := <-ackReceived:
		t.Fatalf("acknowledgement %#v arrived before persistence released it", ack)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseAck)

	select {
	case ack := <-ackReceived:
		if ack.Status != agentprotocol.AckStatusOK || ack.Code != agentprotocol.AckCodePersisted {
			t.Fatalf("ack = %#v, want ok/persisted", ack)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the acknowledgement after releasing persistence")
	}
}
