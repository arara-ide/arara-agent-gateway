package docker

import (
	"context"
	"net/http"
	"net/url"
)

type Stats struct {
	CPUPercent  float64
	MemoryBytes int64
	MemoryLimit int64
}

type RawStats struct {
	CPUStats    CPUSample `json:"cpu_stats"`
	PreCPUStats CPUSample `json:"precpu_stats"`
	MemoryStats struct {
		Usage int64            `json:"usage"`
		Limit int64            `json:"limit"`
		Stats map[string]int64 `json:"stats"`
	} `json:"memory_stats"`
}

type CPUSample struct {
	CPUUsage struct {
		TotalUsage  uint64   `json:"total_usage"`
		PercpuUsage []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	SystemUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs  uint32 `json:"online_cpus"`
}

func (r RawStats) Compute() Stats {
	s := Stats{MemoryLimit: r.MemoryStats.Limit, MemoryBytes: r.MemoryStats.Usage}
	if cache, ok := r.MemoryStats.Stats["inactive_file"]; ok && cache < s.MemoryBytes {
		s.MemoryBytes -= cache
	}
	if r.CPUStats.CPUUsage.TotalUsage < r.PreCPUStats.CPUUsage.TotalUsage || r.CPUStats.SystemUsage <= r.PreCPUStats.SystemUsage {
		return s
	}
	cpus := float64(r.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(r.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpus == 0 {
		cpus = 1
	}
	cpuDelta := float64(r.CPUStats.CPUUsage.TotalUsage - r.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(r.CPUStats.SystemUsage - r.PreCPUStats.SystemUsage)
	s.CPUPercent = cpuDelta / sysDelta * cpus * 100
	return s
}

func (c *Client) ContainerStats(ctx context.Context, id string) (Stats, error) {
	var raw RawStats
	if err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats", url.Values{"stream": {"false"}}, nil, &raw); err != nil {
		return Stats{}, err
	}
	return raw.Compute(), nil
}

func (c *Client) VolumeSizes(ctx context.Context) (map[string]int64, error) {
	var out struct {
		Volumes []struct {
			Name      string
			UsageData struct{ Size int64 }
		}
	}
	if err := c.do(ctx, http.MethodGet, "/system/df", url.Values{"type": {"volume"}}, nil, &out); err != nil {
		return nil, err
	}
	sizes := make(map[string]int64, len(out.Volumes))
	for _, v := range out.Volumes {
		if v.UsageData.Size >= 0 {
			sizes[v.Name] = v.UsageData.Size
		}
	}
	return sizes, nil
}
