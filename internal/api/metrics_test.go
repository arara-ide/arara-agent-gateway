package api_test

import (
	"net/http"
	"testing"

	"github.com/arara-ide/arara-agent-gateway/internal/docker"
	"github.com/arara-ide/arara-agent-gateway/internal/instance"
)

func TestInstanceMetrics(t *testing.T) {
	h := newHarness(t)
	running := h.instance(t, "running")
	stopped := h.instance(t, "stopped")
	if r := h.do(t, "POST", "/api/instances/"+running+"/start", adminKey, nil); r.status != http.StatusOK {
		t.Fatalf("start: %d %s", r.status, r.raw)
	}
	h.env.Engine.SetStats(instance.ContainerName(running), docker.Stats{CPUPercent: 12.5, MemoryBytes: 2048, MemoryLimit: 8192})
	h.env.Engine.SetVolumeSize(instance.VolumeName(running), 4096)
	if r := h.do(t, "GET", "/api/instances/metrics", "", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("metrics without key: %d", r.status)
	}
	r := h.do(t, "GET", "/api/instances/metrics", adminKey, nil)
	if r.status != http.StatusOK {
		t.Fatalf("metrics: %d %s", r.status, r.raw)
	}
	byID := map[string]map[string]any{}
	for _, item := range r.body["instances"].([]any) {
		m := item.(map[string]any)
		byID[m["id"].(string)] = m
	}
	on := byID[running]
	if on["state"] != "running" || on["cpu_percent"] != 12.5 || on["memory_bytes"] != float64(2048) || on["memory_limit_bytes"] != float64(8192) || on["storage_bytes"] != float64(4096) {
		t.Fatalf("running metrics %v", on)
	}
	off := byID[stopped]
	if off["state"] != "stopped" || off["cpu_percent"] != nil || off["memory_bytes"] != nil {
		t.Fatalf("stopped metrics %v", off)
	}
}
