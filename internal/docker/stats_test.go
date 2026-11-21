package docker

import (
	"encoding/json"
	"math"
	"testing"
)

func TestComputeStats(t *testing.T) {
	var raw RawStats
	body := `{"cpu_stats":{"cpu_usage":{"total_usage":300},"system_cpu_usage":2000,"online_cpus":2},"precpu_stats":{"cpu_usage":{"total_usage":100},"system_cpu_usage":1000},"memory_stats":{"usage":1000,"limit":4000,"stats":{"inactive_file":200}}}`
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	s := raw.Compute()
	if math.Abs(s.CPUPercent-40) > 1e-9 || s.MemoryBytes != 800 || s.MemoryLimit != 4000 {
		t.Fatalf("stats %+v", s)
	}
}

func TestComputeStatsWithoutPreviousSample(t *testing.T) {
	var raw RawStats
	raw.CPUStats.CPUUsage.TotalUsage = 50
	raw.CPUStats.SystemUsage = 100
	raw.PreCPUStats.SystemUsage = 100
	raw.CPUStats.CPUUsage.PercpuUsage = []uint64{1, 2}
	raw.MemoryStats.Usage = 10
	raw.MemoryStats.Stats = map[string]int64{"inactive_file": 20}
	if s := raw.Compute(); s.CPUPercent != 0 || s.MemoryBytes != 10 {
		t.Fatalf("stats %+v", s)
	}
}

func TestComputeStatsFallsBackToPerCPUCount(t *testing.T) {
	var raw RawStats
	raw.CPUStats.CPUUsage.TotalUsage = 100
	raw.CPUStats.CPUUsage.PercpuUsage = []uint64{1, 2, 3, 4}
	raw.CPUStats.SystemUsage = 1000
	if s := raw.Compute(); math.Abs(s.CPUPercent-40) > 1e-9 {
		t.Fatalf("stats %+v", s)
	}
}
