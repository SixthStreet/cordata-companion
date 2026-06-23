package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// config holds every knob the daemon exposes. Kept dead simple — a flat
// struct with sensible defaults so most users can run with two values:
// `audio_dir` (where their music lives) and `bearer_token` (so Cordata
// can authenticate).
type config struct {
	// AudioDir is the root of the music collection the companion scans
	// and serves waveforms for. Must be readable by the daemon's user.
	AudioDir string

	// CacheDir is where computed waveform JSON blobs live. One file per
	// audio file, keyed by SHA1 of the audio path. Defaults to
	// `~/.cache/cordata-companion`.
	CacheDir string

	// BindAddress is the TCP socket the HTTP server listens on. Defaults
	// to `:9089`. Bind to a specific interface (`192.168.1.10:9089`) if
	// the host has multiple network interfaces and you want to scope to
	// one.
	BindAddress string

	// BearerToken is the secret Cordata sends in the `Authorization`
	// header. Generated on first run if missing — the daemon writes the
	// generated token back to the config file and prints it to stderr so
	// the user can copy it into Cordata's Settings.
	BearerToken string

	// FfmpegPath lets sites with ffmpeg in a non-standard location point
	// at the binary. Defaults to `ffmpeg` (looked up via $PATH).
	FfmpegPath string

	// FpcalcPath points at the Chromaprint `fpcalc` binary used for
	// acoustic fingerprinting (the `/fingerprint` endpoint). Install via
	// `apt install libchromaprint-tools` on Debian/Ubuntu or
	// `brew install chromaprint` on macOS. Defaults to `fpcalc` (looked
	// up via $PATH).
	FpcalcPath string

	// BpmPath points at the `bpm` binary from bpm-tools, used for tempo
	// detection (the `/bpm` endpoint). Install via `apt install
	// bpm-tools` on Debian/Ubuntu or `brew install bpm-tools` on macOS.
	// Defaults to `bpm` (looked up via $PATH).
	BpmPath string

	// SoxPath points at the `sox` binary used for spectrogram rendering
	// (the `/spectrogram` endpoint). Install via `apt install sox` on
	// Debian/Ubuntu or `brew install sox` on macOS. Defaults to `sox`
	// (looked up via $PATH).
	SoxPath string
}

// loadConfig parses a tiny hand-rolled `key = value` config. We avoid
// pulling in a TOML library to keep the binary lean — the config has
// five keys, all strings, and that's not worth a dependency.
func loadConfig(path string) (*config, error) {
	cfg := &config{
		BindAddress: ":9089",
		FfmpegPath:  "ffmpeg",
		FpcalcPath:  "fpcalc",
		BpmPath:     "bpm",
		SoxPath:     "sox",
	}

	bytes, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// First run: scaffold a config with a generated bearer token
			// and an empty audio_dir for the user to fill in.
			if scaffoldErr := scaffoldConfig(path); scaffoldErr != nil {
				return nil, fmt.Errorf("scaffold default config at %s: %w", path, scaffoldErr)
			}
			return nil, fmt.Errorf("wrote default config at %s — edit audio_dir, then rerun", path)
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	for _, line := range strings.Split(string(bytes), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("bad config line: %q", line)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		// Properly extract the quoted value, ignoring anything after the
		// closing quote (TOML-style inline comments). Falls back to a
		// best-effort strip-to-first-`#` when the value isn't quoted.
		v = extractValue(v)
		switch k {
		case "audio_dir":
			cfg.AudioDir = v
		case "cache_dir":
			cfg.CacheDir = v
		case "bind_address":
			cfg.BindAddress = v
		case "bearer_token":
			cfg.BearerToken = v
		case "ffmpeg_path":
			cfg.FfmpegPath = v
		case "fpcalc_path":
			cfg.FpcalcPath = v
		case "bpm_path":
			cfg.BpmPath = v
		case "sox_path":
			cfg.SoxPath = v
		default:
			return nil, fmt.Errorf("unknown config key %q", k)
		}
	}

	if cfg.CacheDir == "" {
		home, _ := os.UserHomeDir()
		cfg.CacheDir = home + "/.cache/cordata-companion"
	}
	return cfg, nil
}

func (c *config) validate() error {
	if c.AudioDir == "" {
		return errors.New("audio_dir is required")
	}
	if info, err := os.Stat(c.AudioDir); err != nil || !info.IsDir() {
		return fmt.Errorf("audio_dir %q is not a readable directory", c.AudioDir)
	}
	if c.BearerToken == "" {
		return errors.New("bearer_token is required (generate one with `head -c 24 /dev/urandom | xxd -p`)")
	}
	if len(c.BearerToken) < 16 {
		return fmt.Errorf("bearer_token is too short (got %d chars, need ≥ 16)", len(c.BearerToken))
	}
	return nil
}

// scaffoldConfig writes a commented default config + a freshly-generated
// bearer token so first-run users only need to fill in `audio_dir`.
func scaffoldConfig(path string) error {
	if err := os.MkdirAll(parentDir(path), 0o755); err != nil {
		return err
	}
	tok, err := randomToken(24)
	if err != nil {
		return err
	}
	body := fmt.Sprintf(`# cordata-companion config
# Edit audio_dir to point at your HQPlayer music root, then rerun.

audio_dir    = ""
cache_dir    = ""                       # defaults to ~/.cache/cordata-companion
bind_address = ":9089"                  # host:port the HTTP server listens on
bearer_token = "%s"  # generated on first run — paste into Cordata Settings
ffmpeg_path  = "ffmpeg"                 # override only if ffmpeg lives elsewhere
fpcalc_path  = "fpcalc"                 # Chromaprint fpcalc; install libchromaprint-tools
bpm_path     = "bpm"                    # bpm-tools; install bpm-tools
sox_path     = "sox"                    # sox; install sox (for spectrograms)
`, tok)
	return os.WriteFile(path, []byte(body), 0o600)
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func parentDir(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return "."
}

// extractValue parses a TOML-ish right-hand-side: either a quoted string
// (`"..."` / `'...'`) where anything after the closing quote is treated
// as an inline comment and discarded, or an unquoted value where
// everything from the first `#` to end-of-line is stripped.
//
// The previous implementation used `strings.Trim(v, "\"'")` which only
// trims a contiguous run of quote chars from each end. For a value
// like `":9089"                  # host:port` it stripped the leading
// `"` and stopped at the `:`, leaving the trailing `"` + comment in
// the parsed value — which is how an `bind_address` ended up with
// "too many colons" at listen time.
func extractValue(raw string) string {
	if raw == "" {
		return ""
	}
	switch raw[0] {
	case '"', '\'':
		quote := raw[0]
		// Find the matching closing quote. Anything after it is comment.
		if end := strings.IndexByte(raw[1:], quote); end >= 0 {
			return raw[1 : 1+end]
		}
		// Unterminated quote — fall through and treat as unquoted.
	}
	// Unquoted: trim from first '#' onward, then trim whitespace.
	if hash := strings.IndexByte(raw, '#'); hash >= 0 {
		raw = raw[:hash]
	}
	return strings.TrimSpace(raw)
}
