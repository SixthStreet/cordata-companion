package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// scanner walks the audio directory at startup, computing waveforms for
// any audio file without a cached entry, and then watches for new
// files via fsnotify. Modifications + deletions are intentionally not
// handled in v0.1 — the cache's mtime check catches modifications
// lazily (next time Cordata asks for a stale entry it gets recomputed),
// and orphan cache entries are cheap.
type scanner struct {
	root     string
	cache    *cache
	computer *waveformComputer
}

func newScanner(root string, c *cache, comp *waveformComputer) *scanner {
	return &scanner{root: root, cache: c, computer: comp}
}

// audioExtensions enumerates the file extensions the daemon treats as
// audio. Kept conservative — anything ffmpeg can decode that audiophiles
// keep in an HQP library. Add more as needed.
var audioExtensions = map[string]bool{
	".flac": true,
	".wav":  true,
	".aiff": true,
	".aif":  true,
	".alac": true,
	".m4a":  true,
	".mp3":  true,
	".dsf":  true,
	".dff":  true,
}

func isAudioFile(name string) bool {
	return audioExtensions[strings.ToLower(filepath.Ext(name))]
}

// initialScan walks the audio directory and queues compute jobs for any
// audio file that doesn't already have a cache entry. Bounded
// concurrency keeps a quad-core HQP server responsive during the scan
// — the on-demand HTTP path always works in parallel.
func (s *scanner) initialScan(ctx context.Context) error {
	sem := make(chan struct{}, 2) // 2 concurrent ffmpeg subprocesses
	count := 0
	skipped := 0

	walkErr := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // ignore unreadable entries; don't fail the whole scan
		}
		if d.IsDir() || !isAudioFile(d.Name()) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Skip if already cached and fresh.
		if existing, _ := s.cache.load(path); existing != nil {
			skipped++
			return nil
		}
		count++
		sem <- struct{}{}
		go func(audioPath string) {
			defer func() { <-sem }()
			s.processOne(ctx, audioPath)
		}(path)
		return nil
	})

	// Drain the in-flight jobs before returning.
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}
	log.Printf("scanner: initial scan complete — computed=%d, cached=%d", count, skipped)
	return walkErr
}

// watch installs an fsnotify watcher rooted at audio_dir and computes
// waveforms for any newly-created file. Modifications are not handled
// here on purpose — they show up lazily via the cache mtime check.
func (s *scanner) watch(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	// Add the root + every subdir. fsnotify doesn't recurse on its own
	// on Linux/macOS, so we walk once and add each.
	_ = filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = w.Add(path)
		}
		return nil
	})

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				continue
			}
			info, err := os.Stat(ev.Name)
			if err != nil {
				continue
			}
			if info.IsDir() {
				// New subdirectory — extend the watch.
				_ = w.Add(ev.Name)
				continue
			}
			if !isAudioFile(ev.Name) {
				continue
			}
			go s.processOne(ctx, ev.Name)
		case err := <-w.Errors:
			log.Printf("scanner: fsnotify error: %v", err)
		}
	}
}

// processOne computes + caches the waveform for a single file, logging
// failures without propagating. We swallow per-file errors because one
// bad file (e.g. an unsupported codec) shouldn't stop the scan.
func (s *scanner) processOne(ctx context.Context, audioPath string) {
	wf, err := s.computer.compute(ctx, audioPath)
	if err != nil {
		log.Printf("scanner: compute %s: %v", audioPath, err)
		return
	}
	if err := s.cache.store(audioPath, wf); err != nil {
		log.Printf("scanner: cache store %s: %v", audioPath, err)
	}
}
