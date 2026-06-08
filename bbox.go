package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// BboxManager handles interactions with the belabox-receiver Docker container
type BboxManager struct {
	dataDir string
	mu      sync.Mutex
}

func NewBboxManager(dataDir string) *BboxManager {
	return &BboxManager{
		dataDir: dataDir,
	}
}

func (b *BboxManager) runDockerCompose(args ...string) (string, error) {
	// Check if 'docker compose' (v2 plugin) is available (Stage 6)
	useV2 := false
	checkCmd := exec.Command("docker", "compose", "version")
	if err := checkCmd.Run(); err == nil {
		useV2 = true
	}

	cmdArgs := []string{"-f", filepath.Join(b.dataDir, "docker-compose.yml")}
	cmdArgs = append(cmdArgs, args...)

	var cmd *exec.Cmd
	if useV2 {
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

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, stderr.String())
	}
	return out.String(), nil
}

func (b *BboxManager) Start() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err := b.runDockerCompose("up", "-d")
	return err
}

func (b *BboxManager) Stop() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Usando 'down' ao invés de 'stop' para garantir que as configurações 
	// (como mapeamento de portas) sejam recarregadas limpas na próxima vez.
	_, err := b.runDockerCompose("down")
	return err
}

func (b *BboxManager) Restart() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Down e Up garantem a leitura do novo config.json / docker-compose.yml
	b.runDockerCompose("down")
	_, err := b.runDockerCompose("up", "-d")
	return err
}

func (b *BboxManager) GetStatus() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	// docker compose ps --format json (or just simple check)
	out, err := b.runDockerCompose("ps", "--services", "--filter", "status=running")
	if err != nil {
		return "error", err
	}
	if strings.Contains(out, "bbox") {
		return "running", nil
	}
	return "stopped", nil
}

func (b *BboxManager) GetLogs(lines int) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out, err := b.runDockerCompose("logs", "--tail", fmt.Sprintf("%d", lines))
	if err != nil {
		return "", err
	}
	return out, nil
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
