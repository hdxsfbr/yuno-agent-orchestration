package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"yuno/internal/events"
	"yuno/internal/store"
)

func (s *Server) eventsAll(w http.ResponseWriter, r *http.Request) {
	s.streamEvents(w, r, "")
}

func (s *Server) eventsRun(w http.ResponseWriter, r *http.Request) {
	s.streamEvents(w, r, r.PathValue("id"))
}

// streamEvents implements DESIGN 5.12 rule 3: subscribe first (no gap), replay from the
// durable log when Last-Event-ID is present, then stream live with a 15 s heartbeat.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, runID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_flush", "streaming unsupported", nil)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch, cancel := s.hub.Subscribe(runID, 512)
	defer cancel()

	var lastSeq int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		lastSeq, _ = strconv.ParseInt(v, 10, 64)
	}
	if lastSeq > 0 {
		missed, err := store.RunEventsAfter(r.Context(), s.pool, runID, lastSeq, 1000)
		if err != nil {
			s.log.Warn("sse replay failed", "error", err)
		}
		for _, e := range missed {
			writeEvent(w, e)
			if e.Seq > lastSeq {
				lastSeq = e.Seq
			}
		}
	}
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if e.Seq != 0 && e.Seq <= lastSeq {
				continue
			}
			writeEvent(w, e)
			flusher.Flush()
			if e.Seq > lastSeq {
				lastSeq = e.Seq
			}
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, e events.Event) {
	if e.Seq > 0 {
		_, _ = fmt.Fprintf(w, "id: %d\n", e.Seq)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, e.Data)
}
