// Package main is the entry point for cordata-companion, a sidecar
// daemon that precomputes waveform data for audio files on an HQPlayer
// Embedded host and serves it to the Cordata iOS/macOS controller over
// HTTP.
//
// Design intent: stay tiny. One static binary, one config file, one
// HTTP endpoint. No database, no message broker, no cloud auth — the
// audience is HQPe users on home networks who'd rather drop a binary
// on their server than install a 200 MB dependency tree. Friction is
// the filter: users who care about waveform seekbars will run this;
// users who don't won't.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

const version = "0.1.0"

func main() {
	configPath := flag.String("config", defaultConfigPath(), "Path to TOML config file")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("cordata-companion %s\n", version)
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := cfg.validate(); err != nil {
		log.Fatalf("config: %v", err)
	}

	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		log.Fatalf("cache dir: %v", err)
	}

	cache := newCache(cfg.CacheDir)
	computer := &waveformComputer{ffmpegPath: cfg.FfmpegPath, buckets: 2000}

	// Long-lived context cancelled by SIGINT/SIGTERM so all background
	// workers shut down cleanly when the user hits Ctrl-C or systemd
	// sends a stop signal.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Initial scan kicks off in the background so the HTTP server can
	// start accepting requests immediately — the on-demand path always
	// works even if the precompute hasn't caught up yet.
	go func() {
		log.Printf("scanner: initial scan of %s starting", cfg.AudioDir)
		s := newScanner(cfg.AudioDir, cache, computer)
		if err := s.initialScan(ctx); err != nil && ctx.Err() == nil {
			log.Printf("scanner: initial scan error: %v", err)
		}
		log.Printf("scanner: watching for new files")
		if err := s.watch(ctx); err != nil && ctx.Err() == nil {
			log.Printf("scanner: watcher error: %v", err)
		}
	}()

	srv := newServer(cfg, cache, computer)
	log.Printf("cordata-companion %s listening on %s", version, cfg.BindAddress)
	if err := srv.run(ctx); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// defaultConfigPath returns the conventional config location:
// `$XDG_CONFIG_HOME/cordata-companion/config.toml` on Linux, the same
// path under `$HOME/Library/Application Support` on macOS. Users can
// override via `-config` flag.
func defaultConfigPath() string {
	if env := os.Getenv("XDG_CONFIG_HOME"); env != "" {
		return env + "/cordata-companion/config.toml"
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/.config/cordata-companion/config.toml"
	}
	return "config.toml"
}
