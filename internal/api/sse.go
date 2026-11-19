package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/run"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

const heartbeat = 15 * time.Second

func (s *Server) streamRun(w http.ResponseWriter, r *http.Request) error {
	in, err := s.instanceParam(r)
	if err != nil {
		return err
	}
	rr, err := s.Store.GetRun(r.Context(), in.ID, r.PathValue("runId"))
	if err != nil {
		return err
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return newError(http.StatusInternalServerError, "streaming_unsupported", "Streaming is not supported")
	}
	after, _ := strconv.Atoi(r.Header.Get("Last-Event-ID"))
	if after == 0 {
		after, _ = strconv.Atoi(r.URL.Query().Get("after"))
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	hub := s.Runs.Hub(in.ID, rr.ID)
	if hub == nil {
		if rr.Status == store.RunRunning {
			rr.Status, rr.ErrorCode = store.RunInterrupted, "stream_unavailable"
		}
		writeEvent(w, 0, "done", run.RunView(rr))
		flusher.Flush()
		return nil
	}
	replay, ch, unsubscribe := hub.Subscribe(after)
	defer unsubscribe()
	for _, ev := range replay {
		writeEvent(w, ev.ID, ev.Type, ev.Data)
	}
	flusher.Flush()
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			writeEvent(w, ev.ID, ev.Type, ev.Data)
			flusher.Flush()
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, id int, typ string, data any) {
	b, _ := json.Marshal(data)
	if id > 0 {
		_, _ = fmt.Fprintf(w, "id: %d\n", id)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
}
