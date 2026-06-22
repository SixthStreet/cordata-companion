package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// cache stores precomputed waveforms on disk keyed by SHA1 of the
// absolute audio file path. One JSON file per audio file. Stored
// outside the audio directory so we never pollute the user's
// music library with sidecar files.
type cache struct {
	dir string
}

func newCache(dir string) *cache {
	return &cache{dir: dir}
}

// keyFor turns an absolute audio path into a cache filename. SHA1 is
// fine here — this is a lookup key, not a security primitive. The
// content of the JSON file additionally records the source's size +
// mtime so we can invalidate when the audio file is replaced.
func (c *cache) keyFor(audioPath string) string {
	sum := sha1.Sum([]byte(audioPath))
	return hex.EncodeToString(sum[:]) + ".json"
}

func (c *cache) path(audioPath string) string {
	return filepath.Join(c.dir, c.keyFor(audioPath))
}

// load returns the cached waveform for audioPath, or nil if missing or
// stale. Staleness is detected by comparing the recorded source size +
// mtime against the current file's stat. Cheap (one stat) and catches
// the re-encoding case without computing a content hash.
func (c *cache) load(audioPath string) (*waveform, error) {
	bytes, err := os.ReadFile(c.path(audioPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var wf waveform
	if err := json.Unmarshal(bytes, &wf); err != nil {
		return nil, err
	}
	info, err := os.Stat(audioPath)
	if err != nil {
		return nil, nil // source gone — treat as miss, caller will skip
	}
	if wf.SourceSize != info.Size() || wf.SourceMTime != info.ModTime().Unix() {
		return nil, nil // stale
	}
	return &wf, nil
}

// store records the waveform on disk, recording the source stat data
// for future staleness checks.
func (c *cache) store(audioPath string, wf *waveform) error {
	info, err := os.Stat(audioPath)
	if err != nil {
		return err
	}
	wf.SourceSize = info.Size()
	wf.SourceMTime = info.ModTime().Unix()

	body, err := json.Marshal(wf)
	if err != nil {
		return err
	}
	tmp := c.path(audioPath) + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	// Atomic rename so a partial write never leaves a corrupt cache
	// entry that the load path would then try to decode.
	return os.Rename(tmp, c.path(audioPath))
}
