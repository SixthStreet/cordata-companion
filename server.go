package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// server exposes the single HTTP endpoint Cordata talks to. Bearer-
// token auth, JSON responses, conservative timeouts, no fancy routing.
type server struct {
	cfg      *config
	cache    *cache
	computer *waveformComputer
}

func newServer(cfg *config, c *cache, comp *waveformComputer) *server {
	return &server{cfg: cfg, cache: c, computer: comp}
}

func (s *server) run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/waveform", s.requireAuth(s.handleWaveform))
	mux.HandleFunc("/version", s.requireAuth(s.handleVersion))
	mux.HandleFunc("/album-mtimes", s.requireAuth(s.handleAlbumMTimes))
	mux.HandleFunc("/tags", s.requireAuth(s.handleTags))
	mux.HandleFunc("/artwork", s.requireAuth(s.handleArtwork))
	mux.HandleFunc("/dr", s.requireAuth(s.handleDR))
	mux.HandleFunc("/fingerprint", s.requireAuth(s.handleFingerprint))
	mux.HandleFunc("/bpm", s.requireAuth(s.handleBPM))
	mux.HandleFunc("/spectrogram", s.requireAuth(s.handleSpectrogram))
	mux.HandleFunc("/provenance", s.requireAuth(s.handleProvenance))

	srv := &http.Server{
		Addr:              s.cfg.BindAddress,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// requireAuth wraps a handler with a constant-time bearer-token check.
// /healthz is exempted (deliberate — lets a reverse proxy / Docker
// healthcheck verify the daemon is alive without leaking the token).
func (s *server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	expected := []byte("Bearer " + s.cfg.BearerToken)
	return func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"service":"cordata-companion"}`))
}

func (s *server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"version":   version,
		"buckets":   s.computer.buckets,
		"audio_dir": s.cfg.AudioDir,
		"cache_dir": s.cfg.CacheDir,
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// handleWaveform answers `GET /waveform?path=/abs/audio/path`. Returns
// the cached waveform when available; otherwise computes on demand and
// stores. Returns 404 only when the file genuinely doesn't exist —
// "computation failed" is a 500 (unusual enough to be worth surfacing
// vs swallowed as 404).
func (s *server) handleWaveform(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path query parameter required", http.StatusBadRequest)
		return
	}

	// Defense against path traversal: resolve, then ensure the result
	// is rooted in the configured audio_dir. Stops a malicious request
	// from probing `/etc/passwd` even with a stolen token.
	abs, err := filepath.Abs(path)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	rootAbs, _ := filepath.Abs(s.cfg.AudioDir)
	if !strings.HasPrefix(abs+string(filepath.Separator), rootAbs+string(filepath.Separator)) {
		http.Error(w, "path outside audio_dir", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(abs); err != nil {
		http.Error(w, "audio file not found", http.StatusNotFound)
		return
	}

	// Cache hit?
	if wf, err := s.cache.load(abs); err == nil && wf != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cordata-Cache", "hit")
		_ = json.NewEncoder(w).Encode(wf)
		return
	}

	// Compute on demand.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	wf, err := s.computer.compute(ctx, abs)
	if err != nil {
		log.Printf("compute on demand failed for %s: %v", abs, err)
		http.Error(w, "compute failed", http.StatusInternalServerError)
		return
	}
	if err := s.cache.store(abs, wf); err != nil {
		log.Printf("cache store failed for %s: %v", abs, err)
		// Still return the data — caching is best-effort.
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cordata-Cache", "miss")
	_ = json.NewEncoder(w).Encode(wf)
}

// handleAlbumMTimes returns the max file-mtime per album directory under
// `audio_dir`. "Album directory" = the parent directory of an audio file,
// which is the conventional layout (one album per folder). For each such
// directory we report `max(mtime)` across its audio files — the moment
// the album's metadata was most recently touched.
//
// Cordata uses this to drive the "Last Modified" sort in Library view —
// the audiophile-tagging-pass use case where users edit FLAC tags via
// Mp3tag / Tag Editor and want the changed albums to bubble to the top.
//
// Returns: `[{"path": "/srv/music/Album", "mtime": 1719072000}, ...]`.
// Path is the album directory (absolute, server-side); mtime is unix
// seconds.
func (s *server) handleAlbumMTimes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rootAbs, err := filepath.Abs(s.cfg.AudioDir)
	if err != nil {
		http.Error(w, "audio_dir resolve failed", http.StatusInternalServerError)
		return
	}

	type entry struct {
		Path  string `json:"path"`
		MTime int64  `json:"mtime"`
	}
	maxByDir := map[string]int64{}

	walkErr := filepath.WalkDir(rootAbs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; don't fail the whole walk
		}
		if d.IsDir() {
			return nil
		}
		if !isAudioFile(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		dir := filepath.Dir(p)
		mt := info.ModTime().Unix()
		if existing, ok := maxByDir[dir]; !ok || mt > existing {
			maxByDir[dir] = mt
		}
		return nil
	})
	if walkErr != nil {
		log.Printf("album-mtimes walk error: %v", walkErr)
	}

	out := make([]entry, 0, len(maxByDir))
	for path, mt := range maxByDir {
		out = append(out, entry{Path: path, MTime: mt})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
