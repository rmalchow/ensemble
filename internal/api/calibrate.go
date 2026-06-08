package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"ensemble/internal/audio"
	"ensemble/internal/calibrate"
	"ensemble/internal/clock"
	"ensemble/internal/contracts"
	"ensemble/internal/id"
	"ensemble/internal/stream"
)

// CalibStartReq is the POST /api/calibrate body (all optional).
type CalibStartReq struct {
	Volume float64 `json:"volume,omitempty"` // isolation level (default 0.8)
	Loops  int     `json:"loops,omitempty"`  // loop periods per node (default 6)
	// MicDevice selects the capture device on the mic node ("" = system default;
	// an InputDevice ID from the snapshot otherwise, D48).
	MicDevice string `json:"micDevice,omitempty"`
}

// CalibNode is one node's row in the calibration status.
type CalibNode struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Confidence    float64 `json:"confidence"`
	DelayMs       float64 `json:"delayMs"`
	OutputDelayMs int     `json:"outputDelayMs"`
	Used          bool    `json:"used"`
}

// CalibStatus is the GET /api/calibrate response and the live run state. The UI
// polls it while Running to render progress, then reads the final table.
type CalibStatus struct {
	Running     bool        `json:"running"`
	Phase       string      `json:"phase"`
	Index       int         `json:"index"`
	Total       int         `json:"total"`
	CurrentNode string      `json:"currentNode,omitempty"`
	Master      string      `json:"master,omitempty"`
	Mode        string      `json:"mode,omitempty"`
	SpreadMs    float64     `json:"spreadMs,omitempty"`
	Nodes       []CalibNode `json:"nodes"`
	Error       string      `json:"error,omitempty"`
	Done        bool        `json:"done"`
}

// calibManager owns the single-run state behind a mutex (one calibration at a
// time per node).
type calibManager struct {
	mu sync.Mutex
	st CalibStatus
}

func (m *calibManager) status() CalibStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st
}

// tryBegin atomically reserves the manager for a run; false if one is active.
func (m *calibManager) tryBegin(init CalibStatus) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.st.Running {
		return false
	}
	m.st = init
	return true
}

func (m *calibManager) update(fn func(*CalibStatus)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.st)
}

// handleStartCalibrate begins an acoustic-calibration run on THIS node (the mic
// node): it measures every alive member of this node's group and writes their
// outputDelayMs (docs/calibrate.md §6). Proxied like any node-scoped call, so
// the UI POSTs /api/<micNode>/calibrate.
func (s *Server) handleStartCalibrate(c echo.Context) error {
	if s.cfg.Clock == nil {
		return failCode(c, http.StatusNotImplemented, "calibrate", "calibration unavailable on this node")
	}
	if _, ok := s.cfg.Clock.MasterNow(); !ok {
		return failCode(c, http.StatusConflict, "calibrate", "clock not synced yet")
	}

	var req CalibStartReq
	_ = c.Bind(&req)

	self := s.cfg.Cluster.Self()
	snap := s.cfg.Cluster.Snapshot()

	selfNode := findNode(snap, self)
	if selfNode == nil || !hasCap(selfNode.Capabilities.Sources, "input") {
		return failCode(c, http.StatusBadRequest, "calibrate", "this node has no microphone/line-in capability")
	}

	grp := groupOf(snap, self)
	if grp == nil {
		return failCode(c, http.StatusBadRequest, "calibrate", "this node is not in a group")
	}
	members, master, names := aliveMembers(snap, grp)
	if len(members) < 2 {
		return failCode(c, http.StatusBadRequest, "calibrate", "need at least two live group members to calibrate")
	}

	plan := calibrate.Plan{
		Master:     master,
		Members:    members,
		BufferMs:   grp.Settings.BufferMs,
		OrigVolume: memberVolumes(snap, members),
	}
	opt := calibrate.Options{Volume: req.Volume, Loops: req.Loops}

	init := CalibStatus{
		Running: true, Phase: "start", Total: len(members),
		Master: master.String(), Nodes: nodeRows(members, names),
	}
	if !s.calib.tryBegin(init) {
		return failCode(c, http.StatusConflict, "calibrate", "a calibration run is already in progress")
	}

	ctrl := &calibController{client: s.peerClient(), master: master}
	rec := &micRecorder{clock: s.cfg.Clock, mediaDir: s.cfg.MediaDir, device: req.MicDevice}

	go s.runCalibration(plan, opt, ctrl, rec, names)

	return c.JSON(http.StatusAccepted, s.calib.status())
}

// handleGetCalibrate returns the current/last calibration run state.
func (s *Server) handleGetCalibrate(c echo.Context) error {
	return c.JSON(http.StatusOK, s.calib.status())
}

// runCalibration drives calibrate.Run, mirroring progress into the manager and
// folding the final report into the status table.
func (s *Server) runCalibration(plan calibrate.Plan, opt calibrate.Options, ctrl calibrate.Controller, rec calibrate.Recorder, names map[id.ID]string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	progress := func(p calibrate.Progress) {
		s.calib.update(func(st *CalibStatus) {
			st.Phase = p.Phase
			if p.Total > 0 {
				st.Total = p.Total
			}
			switch p.Phase {
			case "measuring":
				st.Index = p.Index
				st.CurrentNode = names[p.Node]
			case "measured":
				for i := range st.Nodes {
					if st.Nodes[i].ID == p.Node.String() {
						st.Nodes[i].Confidence = p.Confidence
					}
				}
			}
		})
	}

	rep, err := calibrate.Run(ctx, ctrl, rec, plan, opt, progress)

	s.calib.update(func(st *CalibStatus) {
		st.Running = false
		st.CurrentNode = ""
		if err != nil {
			st.Phase = "error"
			st.Error = err.Error()
			return
		}
		st.Phase = "done"
		st.Done = true
		st.Mode = rep.Solution.Mode
		st.SpreadMs = rep.Solution.SpreadMs
		// fold measurements + applied delays into the node rows.
		applied := map[id.ID]int{}
		for _, a := range rep.Solution.Applied {
			applied[a.Node] = a.OutputDelayMs
		}
		for _, m := range rep.Measurements {
			for i := range st.Nodes {
				if st.Nodes[i].ID == m.Node.String() {
					st.Nodes[i].Confidence = m.Confidence
					st.Nodes[i].DelayMs = m.DelayMs
					st.Nodes[i].Used = m.Used
					if v, ok := applied[m.Node]; ok {
						st.Nodes[i].OutputDelayMs = v
					}
				}
			}
		}
	})
	if err != nil {
		s.log.Warn("calibration failed", "err", err)
	} else {
		s.log.Info("calibration complete", "mode", rep.Solution.Mode, "spreadMs", rep.Solution.SpreadMs)
	}
}

// ---- controller (drives the group over existing REST endpoints) -------------

type calibController struct {
	client peerClient
	master id.ID
}

func (c *calibController) PlayReference(ctx context.Context) error {
	return c.client.do(ctx, c.master, http.MethodPost, "/api/play", PlayReq{URI: "calibrate:"})
}
func (c *calibController) StopReference(ctx context.Context) error {
	return c.client.do(ctx, c.master, http.MethodPost, "/api/stop", nil)
}
func (c *calibController) SetVolume(ctx context.Context, node id.ID, v float64) error {
	return c.client.do(ctx, node, http.MethodPatch, "/api/node", NodePatchReq{Volume: &v})
}
func (c *calibController) SetOutputDelay(ctx context.Context, node id.ID, ms int) error {
	return c.client.do(ctx, node, http.MethodPatch, "/api/node", NodePatchReq{OutputDelayMs: &ms})
}

// ---- mic recorder (input source + master-clock stamp) -----------------------

// capturePrerollMs of audio is read and DISCARDED after opening a capture
// device, before the measured window: opening a PipeWire/ALSA source emits a
// loud clipped transient (a DC step / buffer pop) for ~the first second that
// otherwise saturates the matched filter. Measured on real hardware (D48).
const capturePrerollMs = 1200

type micRecorder struct {
	clock    contracts.Clock
	mediaDir string
	device   string // capture device id ("" = system default)
}

// Record opens a fresh CONTINUOUS raw capture (not the live input: Source, which
// inserts silence frames when read faster than real time and shreds a recording),
// discards the capture-open transient, then reads exactly d worth of audio into a
// mono buffer stamped with the master-clock time of its first sample. The
// capture-pipe latency between sound and that stamp is constant across nodes
// (same mic, same fresh-open), so it cancels in the pairwise differences the
// solver uses (docs/calibrate.md §4).
func (r *micRecorder) Record(ctx context.Context, d time.Duration) (calibrate.Recording, error) {
	cap, err := audio.OpenRawCapture(ctx, r.device)
	if err != nil {
		return calibrate.Recording{}, err
	}
	defer cap.Close()

	bytesPerSec := stream.SampleRate * stream.Channels * 2

	// discard the capture-open transient (a loud clipped pop for ~the first
	// second on PipeWire/ALSA).
	prerollBytes := capturePrerollMs * bytesPerSec / 1000
	if err := discardN(ctx, cap, prerollBytes); err != nil {
		return calibrate.Recording{}, err
	}

	// stamp the first kept sample in master time, then read d worth.
	t0, ok := r.clock.LocalToMaster(clock.MonoNow())
	if !ok {
		return calibrate.Recording{}, fmt.Errorf("calibrate: clock not synced")
	}
	total := int(d.Seconds()*float64(bytesPerSec)) &^ 3 // whole stereo frames
	raw := make([]byte, total)
	if _, err := io.ReadFull(cap, raw); err != nil {
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			return calibrate.Recording{}, fmt.Errorf("calibrate: capture ended early")
		}
		return calibrate.Recording{}, err
	}

	mono := make([]float32, total/4)
	for i := range mono {
		off := i * 4
		l := int16(binary.LittleEndian.Uint16(raw[off:]))
		rr := int16(binary.LittleEndian.Uint16(raw[off+2:]))
		mono[i] = (float32(l) + float32(rr)) * 0.5 / 32768
	}
	return calibrate.Recording{Mono: mono, T0Master: t0, SampleRate: stream.SampleRate}, nil
}

// discardN reads and drops n bytes, honoring ctx cancellation.
func discardN(ctx context.Context, rd io.Reader, n int) error {
	buf := make([]byte, 16*1024)
	for n > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		chunk := len(buf)
		if chunk > n {
			chunk = n
		}
		m, err := rd.Read(buf[:chunk])
		n -= m
		if err != nil {
			return fmt.Errorf("calibrate: capture ended during warmup: %w", err)
		}
	}
	return nil
}

// ---- peer HTTP client -------------------------------------------------------

// peerClient issues node-addressed REST calls (to a peer or to self over
// loopback), marking them one-hop proxied so the target handles them terminally.
// Mirrors FollowClientImpl's dial strategy.
type peerClient struct {
	cluster Cluster
	http    *http.Client
}

func (s *Server) peerClient() peerClient {
	return peerClient{cluster: s.cfg.Cluster, http: &http.Client{Timeout: 15 * time.Second}}
}

func (p peerClient) do(ctx context.Context, node id.ID, method, path string, body any) error {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}

	snap := p.cluster.Snapshot()
	port := 0
	for _, n := range snap.Nodes {
		if n.ID == node {
			port = n.HTTPPort
			break
		}
	}
	if port == 0 {
		return fmt.Errorf("calibrate: node %s unreachable (no http port)", node)
	}

	addrs := p.cluster.DialCandidates(node)
	if node == p.cluster.Self() {
		// self → loopback; DialCandidates may not include a local address.
		addrs = append([]netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 1})}, addrs...)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("calibrate: node %s unreachable (no address)", node)
	}

	var lastErr error
	for _, a := range addrs {
		url := "http://" + net.JoinHostPort(a.String(), strconv.Itoa(port)) + path
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(proxiedHeader, "1")
		req.Header.Set(fromHeader, p.cluster.Self().String())

		resp, err := p.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		return fmt.Errorf("calibrate: %s %s → status %d", node, path, resp.StatusCode)
	}
	return fmt.Errorf("calibrate: %s %s: %w", node, path, lastErr)
}

// ---- snapshot helpers -------------------------------------------------------

func findNode(snap contracts.Snapshot, n id.ID) *contracts.NodeView {
	for i := range snap.Nodes {
		if snap.Nodes[i].ID == n {
			return &snap.Nodes[i]
		}
	}
	return nil
}

func groupOf(snap contracts.Snapshot, n id.ID) *contracts.GroupView {
	for i := range snap.Groups {
		for _, m := range snap.Groups[i].Members {
			if m == n {
				return &snap.Groups[i]
			}
		}
	}
	return nil
}

// aliveMembers returns the group's alive members (measurement order), its
// master, and an id→name map. The master is kept even though it streams to
// itself — it is a normal measured member (docs/calibrate.md §4).
func aliveMembers(snap contracts.Snapshot, grp *contracts.GroupView) ([]id.ID, id.ID, map[id.ID]string) {
	names := map[id.ID]string{}
	var members []id.ID
	for _, m := range grp.Members {
		n := findNode(snap, m)
		if n == nil || !n.Alive {
			continue
		}
		members = append(members, m)
		names[m] = n.Name
	}
	return members, grp.Master, names
}

func memberVolumes(snap contracts.Snapshot, members []id.ID) map[id.ID]float64 {
	out := make(map[id.ID]float64, len(members))
	for _, m := range members {
		if n := findNode(snap, m); n != nil {
			out[m] = n.Volume
		}
	}
	return out
}

func nodeRows(members []id.ID, names map[id.ID]string) []CalibNode {
	rows := make([]CalibNode, 0, len(members))
	for _, m := range members {
		rows = append(rows, CalibNode{ID: m.String(), Name: names[m]})
	}
	return rows
}

func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}
