package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// BboxManager handles interactions with the belabox-receiver Docker container.
// Caches compose CLI detection once on startup and polls container status lightly
// every 5s instead of spawning multiple docker processes per HTTP request.
type BboxManager struct {
	dataDir      string
	useComposeV2 bool
	cachedStatus atomic.Value // stores string ("running", "stopped", "unknown")
	mu           sync.Mutex
}

func NewBboxManager(dataDir string) *BboxManager {
	useV2 := false
	if err := exec.Command("docker", "compose", "version").Run(); err == nil {
		useV2 = true
	}
	bm := &BboxManager{
		dataDir:      dataDir,
		useComposeV2: useV2,
	}
	bm.cachedStatus.Store("unknown")
	return bm
}

// StartMonitor updates cachedStatus every 5 seconds using lightweight 'docker inspect'
func (b *BboxManager) StartMonitor(ctx context.Context) {
	b.refreshFastStatus()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.refreshFastStatus()
		}
	}
}

func (b *BboxManager) refreshFastStatus() string {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", "bbox").Output()
	if err != nil {
		b.cachedStatus.Store("stopped")
		return "stopped"
	}
	if strings.TrimSpace(string(out)) == "true" {
		b.cachedStatus.Store("running")
		return "running"
	}
	b.cachedStatus.Store("stopped")
	return "stopped"
}

func (b *BboxManager) GetCachedStatus() string {
	if v := b.cachedStatus.Load(); v != nil {
		return v.(string)
	}
	return "unknown"
}

func (b *BboxManager) runDockerCompose(args ...string) (string, error) {
	cmdArgs := []string{"-f", filepath.Join(b.dataDir, "docker-compose.yml")}
	cmdArgs = append(cmdArgs, args...)

	var cmd *exec.Cmd
	if b.useComposeV2 {
		finalArgs := append([]string{"compose"}, cmdArgs...)
		cmd = exec.Command("docker", finalArgs...)
	} else {
		cmd = exec.Command("docker-compose", cmdArgs...)
	}
	cmd.Dir = b.dataDir

	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

func (b *BboxManager) Start() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err := b.runDockerCompose("up", "-d")
	b.refreshFastStatus()
	return err
}

func (b *BboxManager) Stop() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err := b.runDockerCompose("down")
	b.refreshFastStatus()
	return err
}

func (b *BboxManager) Restart() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, _ = b.runDockerCompose("down")
	_, err := b.runDockerCompose("up", "-d")
	b.refreshFastStatus()
	return err
}

// Update pulls the latest configured Docker image from the registry and recreates the bbox container
func (b *BboxManager) Update() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.runDockerCompose("pull"); err != nil {
		return err
	}
	_, err := b.runDockerCompose("up", "-d", "--force-recreate")
	b.refreshFastStatus()
	return err
}

func (b *BboxManager) GetStatus() (string, error) {
	return b.refreshFastStatus(), nil
}

func (b *BboxManager) GetLogs(lines int) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runDockerCompose("logs", "--tail", fmt.Sprintf("%d", lines))
}

func (b *BboxManager) ReadConfig() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	content, err := os.ReadFile(filepath.Join(b.dataDir, "config.json"))
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func (b *BboxManager) WriteConfig(content string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return os.WriteFile(filepath.Join(b.dataDir, "config.json"), []byte(content), 0644)
}

func (b *BboxManager) ReadCompose() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	content, err := os.ReadFile(filepath.Join(b.dataDir, "docker-compose.yml"))
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func (b *BboxManager) WriteCompose(content string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return os.WriteFile(filepath.Join(b.dataDir, "docker-compose.yml"), []byte(content), 0644)
}
