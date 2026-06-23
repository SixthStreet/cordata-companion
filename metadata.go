package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// metadata.go groups the v0.3.0 "deep file inspection" endpoints. Each
// one follows the same shape: validate the path lives under audio_dir,
// shell out to a CLI tool that reads the bytes, return JSON. The
// companion's role is strictly file-bytes-required work — anything that
// can be done over the network (lyrics lookup, AcoustID API queries,
// MBID enrichment) stays in Cordata.

// ---------- /tags ----------

// handleTags answers `GET /tags?path=...`. Dumps every embedded tag the
// file carries via ffprobe — Cordata uses this to surface lyrics,
// ReplayGain, composer, performer, mood, and anything else HQP's
// LibraryGet protocol doesn't expose (it only returns filename + hash +
// length). Cordata picks which fields to render; the companion returns
// everything.
func (s *server) handleTags(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.validatedPath(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	tags, err := readTags(ctx, abs)
	if err != nil {
		http.Error(w, "tag extract failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tags)
}

// readTags runs ffprobe in JSON-output mode and returns a flat
// case-insensitive map of every tag the file carries (both
// format-level and stream-level). ffprobe normalizes case differently
// across containers (FLAC uppercases, MP4 lowercases) — we lowercase
// keys so Cordata can read them by canonical name.
func readTags(ctx context.Context, path string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-hide_banner", "-loglevel", "error",
		"-print_format", "json",
		"-show_format", "-show_streams",
		path,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffprobe: %w (stderr: %s)", err, stderr.String())
	}
	var probe struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
		Streams []struct {
			Tags map[string]string `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &probe); err != nil {
		return nil, fmt.Errorf("parse ffprobe json: %w", err)
	}
	out := map[string]string{}
	for k, v := range probe.Format.Tags {
		out[strings.ToLower(k)] = v
	}
	for _, st := range probe.Streams {
		for k, v := range st.Tags {
			lk := strings.ToLower(k)
			// Don't let stream tags clobber format tags; format wins.
			if _, exists := out[lk]; !exists {
				out[lk] = v
			}
		}
	}
	return out, nil
}

// ---------- /artwork ----------

// handleArtwork answers `GET /artwork?path=...`. Streams the embedded
// cover art bytes (typically a high-res JPEG inside the FLAC's
// METADATA_BLOCK_PICTURE) directly to the client. Auto-detects JPEG vs
// PNG by magic-byte sniffing so Content-Type is correct.
//
// HQPe's `<LibraryGet pictures="1">` returns a downsampled cover
// (~300x300 typically). For Now Playing artwork on a Retina iPad, the
// original embedded image (often 1500x1500+) looks noticeably crisper.
func (s *server) handleArtwork(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.validatedPath(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Extract the first attached picture stream as raw bytes. `-an`
	// strips audio; `-c:v copy` keeps the original encoding (no re-
	// encode, so we get the user's actual embedded image bytes).
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", abs,
		"-an", "-c:v", "copy",
		"-f", "image2pipe",
		"-",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// No embedded art is a 404 (caller knows to fall back), not 500.
		http.Error(w, "no embedded artwork", http.StatusNotFound)
		return
	}

	data := stdout.Bytes()
	w.Header().Set("Content-Type", sniffImageType(data))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

func sniffImageType(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 8 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G':
		return "image/png"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return "application/octet-stream"
}

// ---------- /dr ----------

// handleDR answers `GET /dr?path=...`. Computes Dynamic Range per the
// Pleasurize Music Foundation algorithm — the canonical DR8 / DR12 /
// etc. value audiophiles read off dr.loudness-war.info. Per-track only;
// Cordata aggregates to per-album by averaging.
//
// Algorithm:
//  1. Decode audio to mono f32 PCM at native sample rate
//  2. Split into 3-second blocks
//  3. For each block, record peak (max |sample|) and RMS
//  4. Take the top 20% of blocks by RMS, average their RMS² → RMS_loud
//  5. Take the 2nd-highest peak across all blocks → Peak_2nd
//  6. DR = round( 20·log10(Peak_2nd / RMS_loud) )
//
// We mix to mono before measuring (per-channel DR averaged together
// is the official spec; mono is a close-enough simplification that
// matches what most online DR calculators report).
type drResult struct {
	Version int     `json:"version"`
	DR      int     `json:"dr"`
	PeakDB  float64 `json:"peak_db"`
	RMSDB   float64 `json:"rms_db"`
	Blocks  int     `json:"blocks"`
}

func (s *server) handleDR(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.validatedPath(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	res, err := computeDR(ctx, s.computer.ffmpegPath, abs)
	if err != nil {
		http.Error(w, "dr compute failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func computeDR(ctx context.Context, ffmpegPath, path string) (*drResult, error) {
	// Decode to mono f32 PCM at 44.1k. We don't need original-rate
	// audio for a DR estimate — 44.1k preserves loudness characteristics
	// at way less I/O than 192k for high-res files.
	const sampleRate = 44100
	const blockSeconds = 3
	blockSamples := sampleRate * blockSeconds

	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-i", path,
		"-map", "a:0",
		"-ac", "1",
		"-f", "f32le",
		"-ar", strconv.Itoa(sampleRate),
		"-",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}

	type block struct {
		peak float64
		rms  float64
	}
	var blocks []block

	buf := make([]byte, 4096)
	curIdx := 0
	curPeak := 0.0
	curSumSq := 0.0

	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			n -= n % 4
			for i := 0; i < n; i += 4 {
				bits := binary.LittleEndian.Uint32(buf[i : i+4])
				v := float64(math.Float32frombits(bits))
				av := math.Abs(v)
				if av > curPeak {
					curPeak = av
				}
				curSumSq += v * v
				curIdx++
				if curIdx >= blockSamples {
					blocks = append(blocks, block{
						peak: curPeak,
						rms:  math.Sqrt(curSumSq / float64(curIdx)),
					})
					curIdx = 0
					curPeak = 0
					curSumSq = 0
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = cmd.Wait()
			return nil, fmt.Errorf("read pcm: %w", readErr)
		}
	}
	if curIdx > 0 {
		blocks = append(blocks, block{
			peak: curPeak,
			rms:  math.Sqrt(curSumSq / float64(curIdx)),
		})
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("ffmpeg exit: %w (stderr: %s)", err, stderr.String())
	}
	if len(blocks) < 2 {
		return nil, fmt.Errorf("track too short for DR (%d blocks)", len(blocks))
	}

	// Top 20% of blocks by RMS, averaged in RMS² space.
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].rms > blocks[j].rms })
	topN := len(blocks) / 5
	if topN < 1 {
		topN = 1
	}
	sumSq := 0.0
	for i := 0; i < topN; i++ {
		sumSq += blocks[i].rms * blocks[i].rms
	}
	rmsLoud := math.Sqrt(sumSq / float64(topN))

	// 2nd-highest peak across all blocks.
	peaks := make([]float64, len(blocks))
	for i, b := range blocks {
		peaks[i] = b.peak
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(peaks)))
	peak2nd := peaks[1]

	if rmsLoud <= 0 || peak2nd <= 0 {
		return nil, fmt.Errorf("silent or near-silent track")
	}

	drDB := 20 * math.Log10(peak2nd/rmsLoud)
	return &drResult{
		Version: 1,
		DR:      int(math.Round(drDB)),
		PeakDB:  20 * math.Log10(peak2nd),
		RMSDB:   20 * math.Log10(rmsLoud),
		Blocks:  len(blocks),
	}, nil
}

// ---------- /fingerprint ----------

// handleFingerprint answers `GET /fingerprint?path=...`. Wraps fpcalc
// (from the libchromaprint-tools package) to produce an acoustic
// fingerprint Cordata then sends to the AcoustID API. AcoustID returns
// MusicBrainz recording IDs with confidence scores, which Cordata uses
// to upgrade fuzzy-matched MBIDs to acoustic-confirmed ones — pushes
// our overall match rate from ~80% to ~99%.
type fingerprintResult struct {
	Version     int    `json:"version"`
	Duration    int    `json:"duration"`
	Fingerprint string `json:"fingerprint"`
}

func (s *server) handleFingerprint(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.validatedPath(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	fp, err := computeFingerprint(ctx, s.cfg.FpcalcPath, abs)
	if err != nil {
		http.Error(w, "fingerprint failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(fp)
}

func computeFingerprint(ctx context.Context, fpcalcPath, path string) (*fingerprintResult, error) {
	cmd := exec.CommandContext(ctx, fpcalcPath, "-json", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("fpcalc: %w (stderr: %s)", err, stderr.String())
	}
	// fpcalc's -json output: {"duration": 234.5, "fingerprint": "AQAD..."}
	var raw struct {
		Duration    float64 `json:"duration"`
		Fingerprint string  `json:"fingerprint"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("parse fpcalc json: %w", err)
	}
	return &fingerprintResult{
		Version:     1,
		Duration:    int(math.Round(raw.Duration)),
		Fingerprint: raw.Fingerprint,
	}, nil
}

// ---------- shared helpers ----------

// validatedPath enforces the same audio_dir sandbox the waveform
// endpoint uses. Returns the absolute path on success, or writes an
// HTTP error and returns false on rejection.
func (s *server) validatedPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return "", false
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path query parameter required", http.StatusBadRequest)
		return "", false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return "", false
	}
	rootAbs, _ := filepath.Abs(s.cfg.AudioDir)
	if !strings.HasPrefix(abs+string(filepath.Separator), rootAbs+string(filepath.Separator)) {
		http.Error(w, "path outside audio_dir", http.StatusForbidden)
		return "", false
	}
	return abs, true
}
