// Package spotify is the master-side Spotify bridge (D57): it owns a long-lived
// go-librespot process that advertises a Spotify Connect device (the user's phone
// authenticates over zeroconf and controls playback), watches its event API, and
// drives the group engine — auto-switching this node's group to the "spotify:"
// source when playback starts and back to idle when it stops. go-librespot's audio
// is piped (s16le 44.1 kHz stereo) into a FIFO that the audio package's spotify
// source reads via Attach.
//
// go-librespot is config-driven; we pass everything as `-c field=value` overrides
// (no config file): device_name, the localhost event API (server.*), and the pipe
// audio backend. Verified against go-librespot 0.7.3.
package spotify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"ensemble/internal/contracts"
)

// Config wires a Bridge.
type Config struct {
	BinPath    string       // resolved go-librespot path (CWD-first → $PATH; found by the caller)
	DeviceName string       // advertised Connect name, e.g. "ensemble one"
	StateDir   string       // dir for go-librespot's config_dir + the audio FIFO (the data dir)
	APIPort    int          // localhost event-API port (server.port); 0 → default
	Log        *slog.Logger // nil → slog.Default()
	OnPlay     func()       // Spotify started/resumed → switch this group to spotify:
	OnStop     func()       // Spotify paused/stopped/deselected → idle this group
	OnMetadata func()       // track metadata changed (D57 metadata channel); nil → ignored
}

// DefaultAPIPort is go-librespot's default API port; we enable the API there.
const DefaultAPIPort = 3678

// Bridge owns the go-librespot process, its FIFO pump, and the event client.
type Bridge struct {
	cfg  Config
	fifo string
	log  *slog.Logger

	mu       sync.Mutex
	active   *io.PipeWriter          // current source sink, or nil (discard the pipe)
	cmd      *exec.Cmd               // the go-librespot process (killed on Close)
	fifoFile *os.File                // the pump's FIFO read handle (closed to unblock pump)
	conn     *websocket.Conn         // the live events socket (closed to unblock events)
	meta     contracts.TrackMetadata // latest track metadata (under mu); valid when metaOK
	metaOK   bool                    // a metadata event has arrived this session

	done chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// Latest returns the most recent track metadata, or ok=false if none has arrived
// (no track playing yet). Safe for any goroutine.
func (b *Bridge) Latest() (contracts.TrackMetadata, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.meta, b.metaOK
}

// SetDeviceName renames the advertised Connect device live via go-librespot's
// API (POST /set_device_name) — no process restart. Updates cfg.DeviceName on
// success so later reads/log lines are consistent.
func (b *Bridge) SetDeviceName(name string) error {
	body, _ := json.Marshal(map[string]string{"name": name})
	url := fmt.Sprintf("http://127.0.0.1:%d/set_device_name", b.cfg.APIPort)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("spotify: set_device_name: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("spotify: set_device_name: status %d", resp.StatusCode)
	}
	b.mu.Lock()
	b.cfg.DeviceName = name
	b.mu.Unlock()
	return nil
}

// New builds a Bridge (no process yet — call Run). It creates the audio FIFO and
// go-librespot config dir under StateDir.
func New(cfg Config) (*Bridge, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.APIPort == 0 {
		cfg.APIPort = DefaultAPIPort
	}
	// Per-instance StateDir (the manager gives each bridge its own) may not exist.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("spotify: state dir: %w", err)
	}
	fifo := filepath.Join(cfg.StateDir, "spotify.fifo")
	// A stale FIFO (or plain file) from a prior run is replaced.
	_ = os.Remove(fifo)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		return nil, fmt.Errorf("spotify: mkfifo %s: %w", fifo, err)
	}
	return &Bridge{
		cfg:  cfg,
		fifo: fifo,
		log:  log.With("comp", "spotify"),
		done: make(chan struct{}),
	}, nil
}

// Run launches go-librespot and starts the FIFO pump + event client. Non-blocking.
func (b *Bridge) Run(ctx context.Context) error {
	cfgDir := filepath.Join(b.cfg.StateDir, "go-librespot")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return fmt.Errorf("spotify: config dir: %w", err)
	}
	// Config keys per go-librespot 0.7.3 (koanf): the pipe path is audio_output_pipe
	// (NOT audio_device, which is ALSA-only), and audio_output_pipe_format is a
	// lowercase token — s16le matches the source's rawS16Source (44.1 kHz stereo).
	args := []string{
		"--config_dir", cfgDir,
		"-c", "device_name=" + b.cfg.DeviceName,
		"-c", "server.enabled=true",
		"-c", "server.address=127.0.0.1",
		"-c", fmt.Sprintf("server.port=%d", b.cfg.APIPort),
		"-c", "audio_backend=pipe",
		"-c", "audio_output_pipe=" + b.fifo,
		"-c", "audio_output_pipe_format=s16le",
	}
	cmd := exec.CommandContext(ctx, b.cfg.BinPath, args...)
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	// go-librespot logs to stderr (zeroconf, auth, track changes) — surface it for
	// bring-up; stdout is unused (audio goes to the FIFO via the pipe backend).
	cmd.Stdout, cmd.Stderr = nil, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spotify: start go-librespot: %w", err)
	}
	b.mu.Lock()
	b.cmd = cmd
	b.mu.Unlock()
	b.log.Info("spotify bridge started", "bin", b.cfg.BinPath, "name", b.cfg.DeviceName,
		"apiPort", b.cfg.APIPort, "fifo", b.fifo)

	b.wg.Add(3)
	go func() { defer b.wg.Done(); _ = cmd.Wait() }()
	go b.pump()
	go b.events(ctx)
	return nil
}

// Attach connects a source to the live audio: it returns a reader of go-librespot's
// PCM (s16le 44.1 kHz stereo). The bridge forwards FIFO bytes to the most recent
// Attach; closing the returned reader detaches it (the pump then discards). The
// audio package's openSpotify calls this instead of spawning a process.
func (b *Bridge) Attach() (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	b.mu.Lock()
	if b.active != nil {
		_ = b.active.Close() // supersede a stale attach
	}
	b.active = pw
	b.mu.Unlock()
	return &sourceReader{pr: pr, pw: pw, b: b}, nil
}

// pump reads the FIFO for the bridge's lifetime and forwards to the active source
// (or discards). It opens the FIFO ONCE, O_RDWR|O_NONBLOCK: O_RDWR never blocks on
// open and never EOFs (we hold a writer end, so a stopped go-librespot session does
// not close the pipe), and O_NONBLOCK makes it poller-backed so Read is interrupted
// by Close (the previous O_RDONLY open blocked forever with no writer → the shutdown
// deadlock that needed kill -9).
func (b *Bridge) pump() {
	defer b.wg.Done()
	fd, err := syscall.Open(b.fifo, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		b.log.Warn("spotify fifo open failed", "err", err)
		return
	}
	f := os.NewFile(uintptr(fd), b.fifo)
	b.mu.Lock()
	b.fifoFile = f
	b.mu.Unlock()
	defer f.Close()

	buf := make([]byte, 8192)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			b.mu.Lock()
			w := b.active
			b.mu.Unlock()
			if w != nil {
				if _, werr := w.Write(buf[:n]); werr != nil {
					// the source detached/closed; drop until it (re)attaches.
					b.mu.Lock()
					if b.active == w {
						b.active = nil
					}
					b.mu.Unlock()
				}
			}
		}
		if rerr != nil {
			return // fifo closed (Close) or fatal
		}
	}
}

// events subscribes to go-librespot's /events WebSocket and maps Spotify state to
// the engine callbacks. Reconnects on drop. Unknown event types are logged at debug
// so the exact go-librespot vocabulary is verifiable against real playback.
func (b *Bridge) events(ctx context.Context) {
	defer b.wg.Done()
	u := url.URL{Scheme: "ws", Host: fmt.Sprintf("127.0.0.1:%d", b.cfg.APIPort), Path: "/events"}
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.done:
			return
		default:
		}
		c, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
		if err != nil {
			if b.sleep(time.Second) {
				return
			}
			continue
		}
		b.mu.Lock()
		b.conn = c
		b.mu.Unlock()
		b.log.Debug("spotify events connected")
		for {
			_, data, rerr := c.ReadMessage()
			if rerr != nil {
				break
			}
			b.handleEvent(data)
		}
		b.mu.Lock()
		b.conn = nil
		b.mu.Unlock()
		_ = c.Close()
	}
}

// spotifyEvent is the go-librespot /events envelope. Track info rides on the
// "metadata" event in newer go-librespot AND on the "playing" event in 0.7.3 —
// we capture it from whichever carries a name (see handleEvent).
type spotifyEvent struct {
	Type string `json:"type"`
	Data struct {
		URI           string   `json:"uri"`
		Name          string   `json:"name"`
		ArtistNames   []string `json:"artist_names"`
		AlbumName     string   `json:"album_name"`
		AlbumCoverURL string   `json:"album_cover_url"`
		Duration      int      `json:"duration"` // milliseconds
	} `json:"data"`
}

func (b *Bridge) handleEvent(data []byte) {
	var ev spotifyEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}
	// Capture track metadata from ANY event that carries a name — go-librespot
	// 0.7.3 puts it on "playing", newer versions also emit a dedicated "metadata"
	// event. Relying only on "metadata" left the now-playing bar empty on 0.7.3.
	if ev.Data.Name != "" {
		b.mu.Lock()
		b.meta = contracts.TrackMetadata{
			Title:       ev.Data.Name,
			Artist:      firstOf(ev.Data.ArtistNames),
			Album:       ev.Data.AlbumName,
			ArtURL:      ev.Data.AlbumCoverURL,
			DurationSec: ev.Data.Duration / 1000,
		}
		b.metaOK = true
		b.mu.Unlock()
		if b.cfg.OnMetadata != nil {
			b.cfg.OnMetadata()
		}
	}
	switch ev.Type {
	case "playing":
		b.log.Info("spotify playing", "track", ev.Data.Name, "artist", firstOf(ev.Data.ArtistNames))
		if b.cfg.OnPlay != nil {
			b.cfg.OnPlay()
		}
	case "paused", "stopped", "inactive":
		b.log.Info("spotify stop", "event", ev.Type)
		if b.cfg.OnStop != nil {
			b.cfg.OnStop()
		}
	case "metadata":
		b.log.Debug("spotify metadata", "track", ev.Data.Name, "artist", firstOf(ev.Data.ArtistNames))
	default:
		b.log.Debug("spotify event", "type", ev.Type)
	}
}

// Close stops the event/pump loops and the go-librespot process. Idempotent. It
// explicitly kills the process and closes the FIFO handle + events socket so the
// three goroutines unblock — none of them keys off ctx, so wg.Wait can't hang.
func (b *Bridge) Close() error {
	b.once.Do(func() {
		close(b.done)
		b.mu.Lock()
		if b.cmd != nil && b.cmd.Process != nil {
			_ = b.cmd.Process.Kill() // unblocks the cmd.Wait goroutine
		}
		if b.fifoFile != nil {
			_ = b.fifoFile.Close() // unblocks pump's Read
		}
		if b.conn != nil {
			_ = b.conn.Close() // unblocks events' ReadMessage
		}
		if b.active != nil {
			_ = b.active.Close()
			b.active = nil
		}
		b.mu.Unlock()
		b.wg.Wait()
		_ = os.Remove(b.fifo)
	})
	return nil
}

// sleep waits d or until Close/ctx; returns true if the bridge is shutting down.
func (b *Bridge) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-b.done:
		return true
	case <-t.C:
		return false
	}
}

func firstOf(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

// sourceReader is the audio source's view of the live Spotify stream; Close detaches
// it from the pump so the FIFO bytes are discarded until the next Attach.
type sourceReader struct {
	pr *io.PipeReader
	pw *io.PipeWriter
	b  *Bridge
}

func (s *sourceReader) Read(p []byte) (int, error) { return s.pr.Read(p) }

func (s *sourceReader) Close() error {
	s.b.mu.Lock()
	if s.b.active == s.pw {
		s.b.active = nil
	}
	s.b.mu.Unlock()
	_ = s.pw.Close()
	return s.pr.Close()
}
