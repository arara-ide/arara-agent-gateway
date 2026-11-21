package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/instance"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

const (
	metricsTimeout     = 10 * time.Second
	metricsParallelism = 8
)

type metricsView struct {
	ID               string   `json:"id"`
	State            string   `json:"state"`
	StorageBytes     *int64   `json:"storage_bytes"`
	CPUPercent       *float64 `json:"cpu_percent"`
	MemoryBytes      *int64   `json:"memory_bytes"`
	MemoryLimitBytes *int64   `json:"memory_limit_bytes"`
}

func (s *Server) listMetrics(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.ListInstances(r.Context())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), metricsTimeout)
	defer cancel()
	sizes, sizeErr := s.Engine.VolumeSizes(ctx)
	out := make([]metricsView, len(list))
	sem := make(chan struct{}, metricsParallelism)
	var wg sync.WaitGroup
	for i, in := range list {
		out[i] = metricsView{ID: in.ID, State: string(in.State)}
		if sizeErr == nil {
			if size, ok := sizes[instance.VolumeName(in.ID)]; ok {
				out[i].StorageBytes = &size
			}
		}
		if in.State != store.StateRunning {
			continue
		}
		wg.Add(1)
		go func(v *metricsView, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			st, err := s.Engine.ContainerStats(ctx, instance.ContainerName(id))
			if err != nil {
				return
			}
			v.CPUPercent, v.MemoryBytes, v.MemoryLimitBytes = &st.CPUPercent, &st.MemoryBytes, &st.MemoryLimit
		}(&out[i], in.ID)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"instances": out})
	return nil
}
