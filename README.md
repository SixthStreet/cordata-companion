# cordata-companion

A tiny sidecar daemon for [Cordata](https://github.com/freshangle/cordata)
that precomputes waveform data for your music library and serves it to the
iOS/macOS controller over HTTP. The result is a Roon-style waveform seekbar
in Cordata's Now Playing view — see-at-a-glance quiet vs loud sections,
brick-walled mastering, natural cue points.

Why a sidecar? Cordata is a control point — it sends commands to HQPlayer
Embedded but never sees your audio bits. Computing waveforms requires
reading the audio, so we run a small service next to HQPlayer that does the
reading. It's optional; Cordata works fine without it (you just get the
default progress bar instead of a waveform).

## Install

Drop the binary on the same host that runs HQPlayer Embedded (Linux NUC,
Raspberry Pi, Mac mini — anywhere your music lives).

```bash
# Linux amd64
curl -L -o cordata-companion \
  https://github.com/SixthStreet/cordata-companion/releases/latest/download/cordata-companion-0.1.0-linux-amd64
chmod +x cordata-companion
sudo mv cordata-companion /usr/local/bin/
```

Replacement download URLs for the other platforms (`linux-arm64`,
`darwin-amd64`, `darwin-arm64`, `windows-amd64.exe`) live under the same
release page.

### Dependency: ffmpeg

The daemon shells out to `ffmpeg` to decode audio. Install it via the
host's package manager:

```bash
# Debian / Ubuntu
sudo apt install ffmpeg

# macOS (Homebrew)
brew install ffmpeg

# Arch
sudo pacman -S ffmpeg
```

Most HQPlayer servers already have ffmpeg installed for general audio
duties. If yours doesn't, this is the only dependency you need to add.

## First run

```bash
cordata-companion
```

The first invocation writes a default config file to
`~/.config/cordata-companion/config.toml` and exits with instructions.
Open the file and set `audio_dir` to the root of your music collection:

```toml
audio_dir    = "/srv/music"
cache_dir    = ""                       # defaults to ~/.cache/cordata-companion
bind_address = ":9089"
bearer_token = "<generated automatically>"
ffmpeg_path  = "ffmpeg"
```

Then start the daemon for real:

```bash
cordata-companion
```

On startup it scans `audio_dir` and starts computing waveforms in the
background, two files in parallel. New files added to the library after
startup are picked up via `fsnotify`. The HTTP server starts immediately
— Cordata can request a waveform on-demand even before the initial scan
finishes, and the result is cached for the next request.

## Wiring Cordata

In Cordata on your phone:

1. Settings → HQPlayer → Companion Server
2. Paste the URL: `http://<your-server-hostname>:9089`
3. Paste the bearer token from the config file
4. Tap **Test**

You should see a confirmation that the daemon is reachable. The next
track you play in Cordata gets a waveform seekbar.

## Run as a service

### Linux (systemd)

Save the following to `/etc/systemd/system/cordata-companion.service`:

```ini
[Unit]
Description=cordata-companion sidecar for Cordata
After=network.target

[Service]
ExecStart=/usr/local/bin/cordata-companion
Restart=on-failure
User=hqplayer
Group=hqplayer

[Install]
WantedBy=multi-user.target
```

Then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now cordata-companion
sudo systemctl status cordata-companion
```

### macOS (launchd)

Save to `~/Library/LaunchAgents/net.freshangle.cordata-companion.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>net.freshangle.cordata-companion</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/cordata-companion</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
</dict>
</plist>
```

Then:

```bash
launchctl load ~/Library/LaunchAgents/net.freshangle.cordata-companion.plist
```

## Build from source

```bash
git clone https://github.com/SixthStreet/cordata-companion
cd cordata-companion
make build       # produces ./cordata-companion
make release     # cross-compiles all five platform binaries into dist/
```

Requires Go 1.22 or newer.

## What it does (and doesn't)

**It does:**
- Walks `audio_dir` and computes one waveform per audio file (FLAC, WAV,
  AIFF, ALAC, M4A, MP3, DSF, DFF).
- Caches results to disk so subsequent requests are instant.
- Picks up new files via `fsnotify`.
- Serves waveforms as JSON over HTTP with bearer-token auth.
- Mono mixdown, 2000 peak buckets per track — plenty for a phone-width
  seekbar, payload stays under ~30 KB per track.

**It doesn't (v0.1):**
- Re-watch for *modified* files in realtime. The cache mtime check
  catches modifications lazily the next time a stale entry is requested.
- TLS. Run on a trusted LAN or front it with a reverse proxy if you want
  encryption.
- Multi-directory support. One `audio_dir` per daemon instance.
- Anything beyond waveforms. No metadata enrichment, no artwork sync, no
  transcoding.

## License

MIT. See [LICENSE](./LICENSE).
