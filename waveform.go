package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os/exec"
)

// waveform is the on-disk + over-the-wire shape Cordata consumes. One
// JSON file per audio file. `Channels` is always 1 in v0.1 — we mix
// down to mono before computing peaks, because rendering a stereo
// waveform on a 320pt-wide phone seekbar buys nothing visually and
// doubles the payload. Easy to extend to stereo later by bumping a
// `version` field.
type waveform struct {
	Version  int       `json:"version"`
	Channels int       `json:"channels"`
	Samples  []float32 `json:"samples"`
	// SourceSize / SourceMTime are recorded so the cache layer can
	// invalidate a stale waveform if the audio file gets re-encoded.
	SourceSize  int64 `json:"source_size"`
	SourceMTime int64 `json:"source_mtime_unix"`
}

type waveformComputer struct {
	ffmpegPath string
	buckets    int // target sample count in the output array, e.g. 2000
}

// compute shells out to ffmpeg, decodes the audio to mono float32 PCM
// on stdout, then downsamples to `buckets` peak-amplitude values. We
// use peak (not RMS) because peak is what audiophiles read off a
// seekbar — quiet vs loud sections, brick-walled mastering, etc.
func (w *waveformComputer) compute(ctx context.Context, path string) (*waveform, error) {
	// ffmpeg invocation:
	//   -i <path>          input
	//   -ac 1              downmix to mono
	//   -map a:0           first audio stream (skip cover art "video")
	//   -f f32le           raw 32-bit little-endian float PCM
	//   -ar 8000           resample to 8 kHz — way more than we need
	//                      for a 2000-bucket peak track, keeps the
	//                      pipe small even for hour-long classical.
	//   -                  write to stdout
	cmd := exec.CommandContext(ctx, w.ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-i", path,
		"-map", "a:0",
		"-ac", "1",
		"-f", "f32le",
		"-ar", "8000",
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

	peaks, err := peaksFromStream(stdout, w.buckets)
	if err != nil {
		// Don't lose ffmpeg's stderr — file-format issues report there.
		_ = cmd.Wait()
		return nil, fmt.Errorf("read pcm: %w (ffmpeg: %s)", err, stderr.String())
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("ffmpeg exit: %w (stderr: %s)", err, stderr.String())
	}

	return &waveform{
		Version:  1,
		Channels: 1,
		Samples:  peaks,
	}, nil
}

// peaksFromStream consumes the entire f32le stdout, accumulates the max
// absolute amplitude per bucket, and normalizes to [0, 1] at the end.
//
// Two-pass design: first pass counts total samples (needed to know the
// bucket width), second pass would re-scan. We do it in one streaming
// pass instead by keeping a fixed `buckets`-wide ring and growing each
// bucket's claimed range proportionally — this works because we know
// the desired output length up front. Concretely: we initially treat
// each incoming sample as a single bucket, doubling the bucket width
// (and merging pairs) whenever we'd exceed the buckets target. This is
// the same trick `audiowaveform` uses for progressive resampling.
func peaksFromStream(r io.Reader, buckets int) ([]float32, error) {
	out := make([]float32, 0, buckets)
	bucketWidth := 1 // # of input samples per output bucket; doubles as needed
	idx := 0         // # of input samples consumed since the last bucket flush
	cur := float32(0)
	maxObserved := float32(0)

	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			// Trim trailing partial sample (4 bytes per float32).
			n -= n % 4
			for i := 0; i < n; i += 4 {
				bits := binary.LittleEndian.Uint32(buf[i : i+4])
				v := math.Float32frombits(bits)
				if v < 0 {
					v = -v
				}
				if v > cur {
					cur = v
				}
				idx++
				if idx >= bucketWidth {
					out = append(out, cur)
					if cur > maxObserved {
						maxObserved = cur
					}
					cur = 0
					idx = 0
					if len(out) > buckets {
						// Halve resolution: collapse pairs of buckets
						// into peak-of-pair, double bucketWidth.
						half := make([]float32, (len(out)+1)/2)
						for j := range half {
							a := out[2*j]
							var b float32
							if 2*j+1 < len(out) {
								b = out[2*j+1]
							}
							if a > b {
								half[j] = a
							} else {
								half[j] = b
							}
						}
						out = half
						bucketWidth *= 2
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	// Trailing partial bucket.
	if idx > 0 && len(out) < buckets {
		out = append(out, cur)
		if cur > maxObserved {
			maxObserved = cur
		}
	}

	// Normalize to [0, 1]. Float audio coming out of ffmpeg is already
	// nominally in that range, but clip-heavy masters can push past 1.0;
	// normalize defensively so the renderer never has to clamp.
	if maxObserved > 0 {
		inv := 1.0 / maxObserved
		for i := range out {
			out[i] *= inv
		}
	}
	return out, nil
}

// MarshalJSON would normally be unnecessary, but encoding/json's
// default float32 → float64 widening writes uglier-looking values
// (`0.12345677614212036`) that bloat the payload by ~30%. Keeping the
// default for now — bloat is small enough at 2000 samples — and noting
// here for future tuning.
var _ = json.Marshal
