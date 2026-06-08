package main

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SystemStats struct {
	CPULoad   float64 `json:"cpu_load"` // 0 to 100 percentage
	RAMUsedMB float64 `json:"ram_used_mb"`
	RAMTotalMB float64 `json:"ram_total_mb"`
}

type SysStatsMonitor struct {
	mu            sync.RWMutex
	stats         SystemStats
	lastIdleTime  uint64
	lastTotalTime uint64
}

func NewSysStatsMonitor() *SysStatsMonitor {
	m := &SysStatsMonitor{}
	// Initialize CPU counters to get immediate first read
	m.readCPU()
	return m
}

func (m *SysStatsMonitor) GetStats() SystemStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stats
}

func (m *SysStatsMonitor) Start(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cpu := m.readCPU()
			used, total := m.readRAM()
			
			m.mu.Lock()
			m.stats.CPULoad = cpu
			m.stats.RAMUsedMB = used
			m.stats.RAMTotalMB = total
			m.mu.Unlock()
		}
	}
}

func (m *SysStatsMonitor) readCPU() float64 {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 5 && fields[0] == "cpu" {
			var total uint64
			for i := 1; i < len(fields); i++ {
				v, _ := strconv.ParseUint(fields[i], 10, 64)
				total += v
			}
			idle, _ := strconv.ParseUint(fields[4], 10, 64)

			var cpuLoad float64
			if m.lastTotalTime > 0 && total > m.lastTotalTime && idle >= m.lastIdleTime {
				totalDiff := float64(total - m.lastTotalTime)
				idleDiff := float64(idle - m.lastIdleTime)
				if totalDiff > 0 && totalDiff >= idleDiff {
					cpuLoad = (totalDiff - idleDiff) / totalDiff * 100.0
				}
			}

			m.lastIdleTime = idle
			m.lastTotalTime = total

			return cpuLoad
		}
	}
	return 0
}

func (m *SysStatsMonitor) readRAM() (usedMB, totalMB float64) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer file.Close()

	var total, free, available, buffers, cached uint64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		
		val, _ := strconv.ParseUint(parts[1], 10, 64)
		switch parts[0] {
		case "MemTotal:":
			total = val
		case "MemFree:":
			free = val
		case "MemAvailable:":
			available = val
		case "Buffers:":
			buffers = val
		case "Cached:":
			cached = val
		}
	}

	totalMB = float64(total) / 1024.0
	
	var used uint64
	if available > 0 && total >= available {
		used = total - available
	} else if total > free {
		used = total - free
		if used > buffers {
			used -= buffers
		}
		if used > cached {
			used -= cached
		}
	}
	
	usedMB = float64(used) / 1024.0
	return usedMB, totalMB
}
