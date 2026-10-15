// Package deploytarget is the local release store the deploy_release tool publishes
// to. It enforces idempotency server side (DESIGN 5.4 rule 4): a repeated POST with a
// known Idempotency-Key returns the original release and creates nothing.
package deploytarget

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"yuno/internal/config"
	"yuno/internal/pathcheck"
)

const MaxBodyBytes = 10 * 1024 * 1024

type Release struct {
	ID        string    `json:"release_id"`
	Version   string    `json:"version"`
	Notes     string    `json:"notes"`
	Key       string    `json:"idempotency_key"`
	Files     []string  `json:"files"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
	URL       string    `json:"url"`
	Live      bool      `json:"live"`
}

type createRequest struct {
	Version string `json:"version"`
	Notes   string `json:"notes"`
	Files   []struct {
		Path       string `json:"path"`
		ContentB64 string `json:"content_b64"`
	} `json:"files"`
}

type Store struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	byKey    map[string]*Release
	byID     map[string]*Release
	liveID   string
}

func Open(dir string, maxMB int) (*Store, error) {
	s := &Store{dir: dir, maxBytes: int64(maxMB) * 1024 * 1024, byKey: map[string]*Release{}, byID: map[string]*Release{}}
	if err := os.MkdirAll(filepath.Join(dir, "releases"), 0o755); err != nil {
		return nil, err
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "releases"))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "releases", e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var r Release
		if json.Unmarshal(raw, &r) == nil && r.ID == e.Name() {
			s.byKey[r.Key] = &r
			s.byID[r.ID] = &r
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "live.txt")); err == nil {
		s.liveID = strings.TrimSpace(string(raw))
	}
	return s, nil
}

func (s *Store) list() []*Release {
	out := make([]*Release, 0, len(s.byID))
	for _, r := range s.byID {
		cp := *r
		cp.Live = r.ID == s.liveID
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Create stores a release or returns the existing one for the key (created=false).
func (s *Store) Create(key string, req createRequest) (*Release, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.byKey[key]; ok {
		cp := *r
		cp.Live = r.ID == s.liveID
		return &cp, false, nil
	}
	if strings.TrimSpace(req.Version) == "" {
		return nil, false, errors.New("version is required")
	}
	if len(req.Files) == 0 {
		return nil, false, errors.New("at least one file is required")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, false, err
	}
	rel := &Release{ID: id.String(), Version: req.Version, Notes: req.Notes, Key: key, CreatedAt: time.Now().UTC()}
	rel.URL = "/releases/" + rel.ID + "/"
	base := filepath.Join(s.dir, "releases", rel.ID)
	for _, f := range req.Files {
		if err := pathcheck.Valid(f.Path); err != nil {
			return nil, false, err
		}
		data, err := base64.StdEncoding.DecodeString(f.ContentB64)
		if err != nil {
			return nil, false, fmt.Errorf("file %s: bad base64", f.Path)
		}
		full := filepath.Join(base, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, false, err
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return nil, false, err
		}
		rel.Files = append(rel.Files, f.Path)
		rel.Bytes += int64(len(data))
	}
	meta, _ := json.Marshal(rel)
	if err := os.WriteFile(filepath.Join(base, "meta.json"), meta, 0o644); err != nil {
		return nil, false, err
	}
	s.byKey[key] = rel
	s.byID[rel.ID] = rel
	s.liveID = rel.ID
	_ = os.WriteFile(filepath.Join(s.dir, "live.txt"), []byte(rel.ID), 0o644)
	s.evictLocked()
	cp := *rel
	cp.Live = true
	return &cp, true, nil
}

// evictLocked removes the oldest non-live releases until total bytes fit the cap.
func (s *Store) evictLocked() {
	var total int64
	for _, r := range s.byID {
		total += r.Bytes
	}
	if total <= s.maxBytes {
		return
	}
	all := s.list()
	for i := len(all) - 1; i >= 0 && total > s.maxBytes; i-- {
		r := all[i]
		if r.ID == s.liveID {
			continue
		}
		_ = os.RemoveAll(filepath.Join(s.dir, "releases", r.ID))
		delete(s.byID, r.ID)
		delete(s.byKey, r.Key)
		total -= r.Bytes
	}
}

func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	store, err := Open(cfg.DeployTargetDataDir, cfg.DeployTargetMaxMB)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.DeployTargetListen, Handler: Handler(store, log), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("deploytarget listening", "addr", cfg.DeployTargetListen, "dir", cfg.DeployTargetDataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func Handler(store *Store, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /releases", func(w http.ResponseWriter, _ *http.Request) {
		store.mu.Lock()
		list := store.list()
		store.mu.Unlock()
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST /releases", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" || len(key) > 128 {
			http.Error(w, "Idempotency-Key header is required", http.StatusBadRequest)
			return
		}
		var req createRequest
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		rel, created, err := store.Create(key, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		log.Info("release", "release_id", rel.ID, "created", created, "files", len(rel.Files))
		writeJSON(w, status, rel)
	})
	mux.HandleFunc("GET /releases/{id}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		serveFile(store, w, r, r.PathValue("id"), r.PathValue("path"))
	})
	mux.HandleFunc("GET /live/{path...}", func(w http.ResponseWriter, r *http.Request) {
		store.mu.Lock()
		id := store.liveID
		store.mu.Unlock()
		if id == "" {
			http.Error(w, "no live release", http.StatusNotFound)
			return
		}
		serveFile(store, w, r, id, r.PathValue("path"))
	})
	return mux
}

func serveFile(store *Store, w http.ResponseWriter, r *http.Request, id, p string) {
	store.mu.Lock()
	rel, ok := store.byID[id]
	var cp Release
	if ok {
		cp = *rel
		cp.Live = rel.ID == store.liveID
	}
	store.mu.Unlock()
	if !ok {
		http.Error(w, "release not found", http.StatusNotFound)
		return
	}
	if p == "" {
		writeJSON(w, http.StatusOK, &cp)
		return
	}
	if err := pathcheck.Valid(p); err != nil || p == "meta.json" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	full := filepath.Join(store.dir, "releases", id, filepath.FromSlash(p))
	f, err := os.Open(full)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// ServeContent, not ServeFile: ServeFile redirects paths ending in /index.html.
	http.ServeContent(w, r, filepath.Base(p), st.ModTime(), f)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
