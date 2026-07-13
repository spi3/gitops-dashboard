// t032-e2e is a reproducible launched-process verification harness for T-032.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/storage"
	"github.com/gorilla/websocket"
)

func main() {
	root, err := os.MkdirTemp("", "t032-e2e-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(root)
	data := filepath.Join(root, "data")
	store, err := storage.Open(filepath.Join(data, "gitops-dashboard.db"))
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	scan, err := store.StartScan(ctx, "manual")
	if err != nil {
		log.Fatal(err)
	}
	service := core.Service{ID: "manual-web", Name: "web", Repository: "manual", SourceCommit: "manual", SourcePath: "docker_files/manual-agent/web/docker-compose.yml", Runtime: "compose", Health: core.HealthUnknown}
	if err := store.FinishScan(ctx, scan, "manual", "manual", []core.Service{service}, nil); err != nil {
		log.Fatal(err)
	}
	if err := store.Close(); err != nil {
		log.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	config := "server:\n  listen: 127.0.0.1:18106\n  dataDir: " + data + "\n  repoCacheDir: " + filepath.Join(root, "repos") + "\nauth:\n  mode: dev-no-auth\n  agent:\n    tokens: [t032-token]\nmonitoring:\n  defaultInterval: 1s\nruntime:\n  docker:\n    - name: manual-agent\n      kind: agent\n      interval: 1s\n"
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		log.Fatal(err)
	}
	cmd := exec.Command("/tmp/t032-current-dashboard", "-config", configPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	url := "http://127.0.0.1:18106/api/summary"
	var before, after, expired core.DashboardSummary
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if summary, err := get(url); err == nil {
			before = summary
			break
		}
		if time.Now().After(deadline) {
			log.Fatal("server did not start")
		}
	}
	ws, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:18106/api/agents/connect", http.Header{"X-Agent-Token": []string{"t032-token"}})
	if err != nil {
		log.Fatal(err)
	}
	err = ws.WriteJSON(core.AgentMessage{Target: "manual-agent", Containers: []core.ContainerStatus{{Name: "/web-1", State: "running", Status: "Up 1 minute", Labels: map[string]string{core.DockerComposeProjectLabel: "web", core.DockerComposeServiceLabel: "web"}}}})
	_ = ws.Close()
	if err != nil {
		log.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if summary, err := get(url); err == nil && len(summary.Statuses) == 1 && summary.Statuses[0].Health == core.HealthHealthy {
			after = summary
			break
		}
		if time.Now().After(deadline) {
			log.Fatal("agent report did not become healthy")
		}
	}
	time.Sleep(2200 * time.Millisecond)
	expired, err = get(url)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("beforeStatuses=%d after=%s expired=%s\n", len(before.Statuses), after.Statuses[0].Health, expired.Statuses[0].Health)
}

func get(url string) (core.DashboardSummary, error) {
	res, err := http.Get(url)
	if err != nil {
		return core.DashboardSummary{}, err
	}
	defer res.Body.Close()
	var summary core.DashboardSummary
	return summary, json.NewDecoder(res.Body).Decode(&summary)
}
