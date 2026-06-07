// Command dumbclient is a protocol-minimal, receive-only ensemble audio
// participant — the reference implementation for docs/DUMB-CLIENT.md.
//
// It is NOT a cluster member: it never gossips, never appears in /api/cluster,
// and is invisible to codec negotiation. It speaks ONLY the v1 wire protocol
// (magic 0xE5): it subscribes to a master's SOURCE_PORT, follows the master
// clock on STREAM_PORT, and schedules PCM playout against the master clock —
// exactly as a tiny firmware client (ESP32-S3) would, minus the codec.
//
// Deliberately standalone: it imports NOTHING from internal/ (only the Go
// stdlib). If this file is sufficient to play in sync, the spec is sufficient.
//
// Scope vs. real firmware: this client is PCM-only. Opus is for real firmware
// (libopus); the reference covers the PROTOCOL, not the codec. Pin the group
// codec to pcm (or run during a pcm window) before pointing it at a group.
// Use --transport tcp on Wi-Fi: a 3864-byte PCM datagram IP-fragments into ~3
// packets and loses catastrophically on lossy links.
//
// Usage:
//
//	dumbclient --node 127.0.0.1:18080 [--group <idOrName>] [--out null|exec]
//	dumbclient --source <ip:port> --clock <ip:port> [--transport udp|tcp] ...
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"
)

// ---- v1 wire protocol constants (docs/DUMB-CLIENT.md) ----------------------

const (
	magic      = 0xE5 // version marker: unknown types are ignored; a new magic = incompatible revision
	headerSize = 24   // magic|type|gen|seq|pts|payloadLen

	typeAudio    = 0x01
	typeFEC      = 0x02 // optional; this client ignores it (gaps -> silence)
	typeClockReq = 0x10
	typeClockRsp = 0x11
	typeHello    = 0x20
	typeBye      = 0x21
	typeRestart  = 0x22
	typeReconfig = 0x23

	flagPrimeMe = 0x01 // HELLO/RESTART payload: please burst-prime me
	flagStop    = 0x01 // RECONFIG payload: end-of-session

	frameBytes = 3840 // 48k stereo s16le 20ms
	frameNanos = 20 * 1_000_000
)

// Timing constants — must match the server (internal/stream, internal/source).
const (
	helloRetries       = 3
	helloRetryInterval = 500 * time.Millisecond
	keepaliveInterval  = 5 * time.Second
	clockInterval      = 1 * time.Second
	watchdogTimeout    = 2 * time.Second
	discoveryInterval  = 5 * time.Second
	clockBest          = 5  // median of the best-RTT N
	clockWindow        = 30 // of the last N samples
)

// ---- header --------------------------------------------------------------

type header struct {
	magic, typ byte
	gen        uint32
	seq        uint64
	pts        int64
	payloadLen uint16
}

// parseHeader decodes the fixed 24-byte big-endian frame header. It does NOT
// validate magic/type (callers do). Returns ok=false if buf is too short.
func parseHeader(buf []byte) (header, bool) {
	if len(buf) < headerSize {
		return header{}, false
	}
	return header{
		magic:      buf[0],
		typ:        buf[1],
		gen:        binary.BigEndian.Uint32(buf[2:6]),
		seq:        binary.BigEndian.Uint64(buf[6:14]),
		pts:        int64(binary.BigEndian.Uint64(buf[14:22])),
		payloadLen: binary.BigEndian.Uint16(buf[22:24]),
	}, true
}

// encodeFrame writes header + payload into a fresh buffer.
func encodeFrame(typ byte, gen uint32, seq uint64, pts int64, payload []byte) []byte {
	b := make([]byte, headerSize+len(payload))
	b[0] = magic
	b[1] = typ
	binary.BigEndian.PutUint32(b[2:6], gen)
	binary.BigEndian.PutUint64(b[6:14], seq)
	binary.BigEndian.PutUint64(b[14:22], uint64(pts))
	binary.BigEndian.PutUint16(b[22:24], uint16(len(payload)))
	copy(b[headerSize:], payload)
	return b
}

// ---- clock follower (NTP-style, best-of-N median) --------------------------

type sample struct{ offset, rtt int64 }

// computeSample derives the per-exchange offset and rtt from the four NTP
// timestamps (all ns). offset = master_ns - local_ns.
//
//	offset = ((t2 - t1) + (t3 - t4)) / 2
//	rtt    = (t4 - t1) - (t3 - t2)
func computeSample(t1, t2, t3, t4 int64) sample {
	return sample{
		offset: ((t2 - t1) + (t3 - t4)) / 2,
		rtt:    (t4 - t1) - (t3 - t2),
	}
}

// medianOffset returns the median offset of the best-RTT `best` samples drawn
// from the last `window` of ss, and whether an estimate exists. This mirrors
// the server's estimator exactly so the two agree on the offset.
func medianOffset(ss []sample, window, best int) (int64, bool) {
	if len(ss) == 0 {
		return 0, false
	}
	if len(ss) > window {
		ss = ss[len(ss)-window:]
	}
	byRTT := append([]sample(nil), ss...)
	sort.Slice(byRTT, func(i, j int) bool { return byRTT[i].rtt < byRTT[j].rtt })
	n := best
	if n > len(byRTT) {
		n = len(byRTT)
	}
	offs := make([]int64, n)
	for i := 0; i < n; i++ {
		offs[i] = byRTT[i].offset
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	return offs[(len(offs)-1)/2], true // lower-middle median (integer-only)
}

// monoEpoch anchors a monotonic clock. EVERY local-time value (clock t1/t4,
// playout deadlines) MUST come from this one clock.
var monoEpoch = time.Now()

func monoNow() int64 { return int64(time.Since(monoEpoch)) }

type clockFollower struct {
	conn *net.UDPConn

	mu      sync.Mutex
	master  *net.UDPAddr
	gen     uint32
	seq     uint64
	pending map[uint64]int64 // seq -> t1
	samples []sample
	synced  bool
}

func newClockFollower(conn *net.UDPConn) *clockFollower {
	return &clockFollower{conn: conn, pending: make(map[uint64]int64)}
}

// setMaster (re)points the follower. An ENDPOINT change wipes the window; a
// mere gen bump keeps it (the master process/clock is unchanged).
func (c *clockFollower) setMaster(addr *net.UDPAddr, gen uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	endpointChanged := c.master == nil || c.master.String() != addr.String()
	c.master = addr
	c.gen = gen
	clear(c.pending)
	if endpointChanged {
		c.samples = c.samples[:0]
		c.synced = false
	}
}

func (c *clockFollower) probe() {
	c.mu.Lock()
	if c.master == nil {
		c.mu.Unlock()
		return
	}
	t1 := monoNow()
	seq := c.seq
	c.seq++
	gen := c.gen
	dst := c.master
	c.pending[seq] = t1
	// prune lost replies (> 5s)
	for s, ts := range c.pending {
		if t1-ts > int64(5*time.Second) {
			delete(c.pending, s)
		}
	}
	c.mu.Unlock()

	pkt := encodeFrame(typeClockReq, gen, seq, 0, make([]byte, 24)) // t1|t2|t3, t1 unused by server
	_, _ = c.conn.WriteToUDP(pkt, dst)
}

// onReply feeds a 0x11 reply. t4 must be stamped by the caller on arrival.
func (c *clockFollower) onReply(h header, payload []byte, t4 int64) {
	if len(payload) < 24 {
		return
	}
	t2 := int64(binary.BigEndian.Uint64(payload[8:16]))
	t3 := int64(binary.BigEndian.Uint64(payload[16:24]))
	c.mu.Lock()
	defer c.mu.Unlock()
	if h.gen != c.gen {
		return
	}
	t1, ok := c.pending[h.seq]
	if !ok {
		return
	}
	delete(c.pending, h.seq)
	c.samples = append(c.samples, computeSample(t1, t2, t3, t4))
	if len(c.samples) > clockWindow {
		c.samples = c.samples[len(c.samples)-clockWindow:]
	}
	if !c.synced {
		c.synced = true
		log.Printf("clock sync acquired master=%s gen=%d", c.master, c.gen)
	}
}

// offset returns the current master-local offset (ns) and whether synced.
func (c *clockFollower) offset() (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return medianOffset(c.samples, clockWindow, clockBest)
}

// masterToLocal converts a master-clock instant to a local monotonic instant.
func (c *clockFollower) masterToLocal(masterNs int64) (int64, bool) {
	off, ok := c.offset()
	if !ok {
		return 0, false
	}
	return masterNs - off, true
}

// ---- jitter buffer + playout scheduler -------------------------------------

type pslot struct {
	pts     int64
	payload []byte
}

type playout struct {
	out output

	mu        sync.Mutex
	slots     map[uint64]pslot
	gen       uint32
	nextSeq   uint64
	hasNext   bool
	originSeq uint64
	originPTS int64
	bufferNs  int64

	played, silence, late uint64
	lastFrame             int64 // monoNow of last accepted frame
	gotFrame              bool
}

func newPlayout(out output, bufferMs int) *playout {
	return &playout{
		out:      out,
		slots:    make(map[uint64]pslot),
		bufferNs: int64(bufferMs) * 1_000_000,
	}
}

// reset arms for a new generation (RECONFIG/gen change). The next frame fixes
// the seq/pts origin.
func (p *playout) reset(gen uint32, bufferMs int) {
	p.mu.Lock()
	p.slots = make(map[uint64]pslot)
	p.gen = gen
	p.hasNext = false
	p.bufferNs = int64(bufferMs) * 1_000_000
	p.gotFrame = false
	p.mu.Unlock()
}

func (p *playout) setBufferMs(ms int) {
	p.mu.Lock()
	p.bufferNs = int64(ms) * 1_000_000
	p.mu.Unlock()
}

// push enqueues one delivered audio frame (already gen-filtered by the caller).
func (p *playout) push(seq uint64, pts int64, payload []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.hasNext {
		p.nextSeq = seq
		p.originSeq = seq
		p.originPTS = pts
		p.hasNext = true
	}
	if seq < p.nextSeq {
		p.late++
		return
	}
	cp := append([]byte(nil), payload...)
	p.slots[seq] = pslot{pts: pts, payload: cp}
	p.lastFrame = monoNow()
	p.gotFrame = true
}

// slotPTS derives a seq's master-clock pts from the session origin (so gaps
// schedule at the right instant even with no frame present).
func (p *playout) slotPTS(seq uint64) int64 {
	return p.originPTS + int64(seq-p.originSeq)*frameNanos
}

// run is the scheduler: one output frame per slot, in seq order, asleep until
// each slot's local deadline. Gaps play silence; it never blocks on the clock
// being unsynced (it waits).
func (p *playout) run(ctx context.Context, clk *clockFollower) {
	silence := make([]byte, frameBytes)
	for {
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		if !p.hasNext {
			p.mu.Unlock()
			sleep(ctx, 5*time.Millisecond)
			continue
		}
		seq := p.nextSeq
		target := p.slotPTS(seq) + p.bufferNs
		p.mu.Unlock()

		local, ok := clk.masterToLocal(target)
		if !ok {
			sleep(ctx, 5*time.Millisecond) // unsynced gate
			continue
		}
		if d := local - monoNow(); d > 0 {
			sleep(ctx, time.Duration(d))
		}

		p.mu.Lock()
		s, have := p.slots[seq]
		delete(p.slots, seq)
		// A slot that is already a full frame late must not consume device time.
		if monoNow() > local+frameNanos {
			if have {
				p.late++
			}
			p.nextSeq++
			p.mu.Unlock()
			continue
		}
		var buf []byte
		if have {
			buf = s.payload
			p.played++
		} else {
			buf = silence
			p.silence++
		}
		p.nextSeq++
		p.mu.Unlock()

		if err := p.out.write(buf); err != nil {
			log.Printf("output write failed: %v", err)
		}
	}
}

// starved reports whether no fresh frame has arrived for watchdogTimeout.
func (p *playout) starved() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gotFrame && monoNow()-p.lastFrame > int64(watchdogTimeout)
}

func (p *playout) stats() (played, silence, late, buffered uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.played, p.silence, p.late, uint64(len(p.slots))
}

// ---- output backends -------------------------------------------------------

type output interface {
	write([]byte) error
	Close() error
}

type nullOutput struct{}

func (nullOutput) write([]byte) error { return nil }
func (nullOutput) Close() error       { return nil }

// execOutput pipes raw s16le frames to an external player (pw-play/aplay/ffplay).
type execOutput struct {
	cmd *exec.Cmd
	w   io.WriteCloser
}

func newExecOutput() (*execOutput, error) {
	// pw-play first (PipeWire), then aplay (ALSA) as a fallback.
	var path string
	for _, c := range []string{"pw-play", "aplay", "ffplay"} {
		if p, err := exec.LookPath(c); err == nil {
			path = p
			break
		}
	}
	if path == "" {
		return nil, errors.New("no exec player found (pw-play/aplay/ffplay)")
	}
	var args []string
	switch filepathBase(path) {
	case "pw-play":
		args = []string{"--rate=48000", "--channels=2", "--format=s16", "-"}
	case "aplay":
		args = []string{"-f", "S16_LE", "-r", "48000", "-c", "2", "-"}
	default: // ffplay
		args = []string{"-f", "s16le", "-ar", "48000", "-ch_layout", "stereo", "-nodisp", "-"}
	}
	cmd := exec.Command(path, args...)
	cmd.Stderr = os.Stderr
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	log.Printf("exec output: %s %v", path, args)
	return &execOutput{cmd: cmd, w: w}, nil
}

func (e *execOutput) write(b []byte) error { _, err := e.w.Write(b); return err }
func (e *execOutput) Close() error {
	_ = e.w.Close()
	return e.cmd.Wait()
}

func filepathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// ---- discovery (GET /api/cluster) ------------------------------------------

type clusterDoc struct {
	Nodes []struct {
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		Addrs      []string `json:"addrs"`
		StreamPort int      `json:"streamPort"`
		SourcePort int      `json:"sourcePort"`
		Observed   map[string]struct {
			IP string `json:"ip"`
		} `json:"observed"`
	} `json:"nodes"`
	Groups []struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		Master   string   `json:"master"`
		Members  []string `json:"members"`
		Settings struct {
			Codec     string `json:"codec"`
			Transport string `json:"transport"`
			BufferMs  int    `json:"bufferMs"`
		} `json:"settings"`
	} `json:"groups"`
}

// target is the resolved endpoint set to subscribe + clock against.
type target struct {
	master     string // node id
	host       string // dial IP
	sourcePort int
	streamPort int
	transport  string
	bufferMs   int
	codec      string
}

// resolve picks the group (by id/name, or the first non-empty one) and returns
// the master's endpoints. It mirrors the daemon's dial-IP choice: the first
// observed IP, else the first /addr CIDR's host.
func resolve(doc *clusterDoc, groupSel string) (target, error) {
	var g *struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		Master   string   `json:"master"`
		Members  []string `json:"members"`
		Settings struct {
			Codec     string `json:"codec"`
			Transport string `json:"transport"`
			BufferMs  int    `json:"bufferMs"`
		} `json:"settings"`
	}
	for i := range doc.Groups {
		gr := &doc.Groups[i]
		if groupSel != "" {
			if gr.ID == groupSel || gr.Name == groupSel {
				g = gr
				break
			}
			continue
		}
		if len(gr.Members) > 0 {
			g = gr
			break
		}
	}
	if g == nil {
		return target{}, errors.New("no matching group")
	}
	for i := range doc.Nodes {
		n := &doc.Nodes[i]
		if n.ID != g.Master {
			continue
		}
		host := dialHost(n.Observed, n.Addrs)
		if host == "" {
			return target{}, errors.New("master has no dial address yet")
		}
		return target{
			master:     g.Master,
			host:       host,
			sourcePort: n.SourcePort,
			streamPort: n.StreamPort,
			transport:  g.Settings.Transport,
			bufferMs:   g.Settings.BufferMs,
			codec:      g.Settings.Codec,
		}, nil
	}
	return target{}, errors.New("master node not found in snapshot")
}

func dialHost(observed map[string]struct {
	IP string `json:"ip"`
}, addrs []string) string {
	for _, o := range observed {
		if o.IP != "" {
			return o.IP
		}
	}
	for _, a := range addrs {
		if h, _, err := net.SplitHostPort(a); err == nil {
			return h
		}
		// a CIDR "10.0.0.5/24" -> host part
		for i := 0; i < len(a); i++ {
			if a[i] == '/' {
				return a[:i]
			}
		}
		return a
	}
	return ""
}

func fetchCluster(ctx context.Context, nodeURL string) (*clusterDoc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+nodeURL+"/api/cluster", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cluster: status %d", resp.StatusCode)
	}
	var doc clusterDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// ---- generation accessor ---------------------------------------------------

func (c *clockFollower) genFor() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// ---- main ------------------------------------------------------------------

func main() {
	log.SetFlags(log.Ltime)

	node := flag.String("node", "", "discovery: ensemble node host:httpPort (poll /api/cluster)")
	source := flag.String("source", "", "fixed: master SOURCE ip:port (with --clock)")
	clockAddr := flag.String("clock", "", "fixed: master STREAM/clock ip:port (with --source)")
	group := flag.String("group", "", "group id or name to follow (default: first non-empty)")
	transport := flag.String("transport", "udp", "udp|tcp (use tcp on Wi-Fi)")
	outKind := flag.String("out", "null", "null|exec")
	bufferMs := flag.Int("buffer-ms", 150, "playout lead (overridden by group setting in --node mode)")
	flag.Parse()

	if *node == "" && (*source == "" || *clockAddr == "") {
		log.Fatal("need --node OR (--source AND --clock)")
	}

	var out output = nullOutput{}
	if *outKind == "exec" {
		eo, err := newExecOutput()
		if err != nil {
			log.Fatalf("exec output: %v", err)
		}
		out = eo
	}
	defer out.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go waitSignal(cancel)

	// One UDP socket for clock probes/replies AND inbound audio (the mux model).
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		log.Fatalf("udp listen: %v", err)
	}
	defer udp.Close()

	clk := newClockFollower(udp)
	pl := newPlayout(out, *bufferMs)

	app := &client{
		udp:       udp,
		clk:       clk,
		pl:        pl,
		transport: *transport,
		bufferMs:  *bufferMs,
		group:     *group,
	}

	go app.readUDP(ctx)
	go clk.loop(ctx)
	go pl.run(ctx, clk)
	go app.statsLoop(ctx)
	go app.watchdogLoop(ctx)

	if *node != "" {
		app.discoveryLoop(ctx, *node)
	} else {
		t, err := fixedTarget(*source, *clockAddr, *transport, *bufferMs)
		if err != nil {
			log.Fatal(err)
		}
		app.point(t, 0)
		go app.keepaliveLoop(ctx)
		<-ctx.Done()
	}
}

// client ties the pieces together and owns the active subscription state.
type client struct {
	udp       *net.UDPConn
	clk       *clockFollower
	pl        *playout
	transport string
	bufferMs  int
	group     string

	mu      sync.Mutex
	cur     target
	haveCur bool
	srcAddr *net.UDPAddr
	tcpConn net.Conn
	tcpGen  uint32
}

func fixedTarget(source, clockAddr, transport string, bufferMs int) (target, error) {
	sh, sp, err := net.SplitHostPort(source)
	if err != nil {
		return target{}, fmt.Errorf("--source: %w", err)
	}
	ch, cp, err := net.SplitHostPort(clockAddr)
	if err != nil {
		return target{}, fmt.Errorf("--clock: %w", err)
	}
	_ = ch
	return target{
		host:       sh,
		sourcePort: atoiPort(sp),
		streamPort: atoiPort(cp),
		transport:  transport,
		bufferMs:   bufferMs,
		codec:      "pcm",
	}, nil
}

func atoiPort(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// discoveryLoop polls /api/cluster, re-resolving the master every 5s and
// resubscribing on any endpoint change.
func (c *client) discoveryLoop(ctx context.Context, nodeURL string) {
	go c.keepaliveLoop(ctx)
	t := time.NewTicker(discoveryInterval)
	defer t.Stop()
	c.pollOnce(ctx, nodeURL)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.pollOnce(ctx, nodeURL)
		}
	}
}

func (c *client) pollOnce(ctx context.Context, nodeURL string) {
	doc, err := fetchCluster(ctx, nodeURL)
	if err != nil {
		log.Printf("discovery: %v", err)
		return
	}
	t, err := resolve(doc, c.group)
	if err != nil {
		return // no group/master yet; keep current
	}
	if t.codec != "" && t.codec != "pcm" {
		log.Printf("WARNING: group codec=%s; this PCM-only client cannot decode it (pin pcm)", t.codec)
	}
	c.mu.Lock()
	same := c.haveCur && c.cur.host == t.host && c.cur.sourcePort == t.sourcePort &&
		c.cur.streamPort == t.streamPort && c.cur.transport == t.transport
	c.mu.Unlock()
	if same {
		c.pl.setBufferMs(t.bufferMs)
		return
	}
	c.point(t, 0)
}

// point (re)subscribes to a new master endpoint set under generation gen.
func (c *client) point(t target, gen uint32) {
	// Honor the group transport unless overridden on the CLI (fixed mode).
	if t.transport == "" {
		t.transport = c.transport
	}
	srcAddr := &net.UDPAddr{IP: net.ParseIP(t.host), Port: t.sourcePort}
	clkAddr := &net.UDPAddr{IP: net.ParseIP(t.host), Port: t.streamPort}

	c.mu.Lock()
	oldTCP := c.tcpConn
	c.tcpConn = nil
	c.cur = t
	c.haveCur = true
	c.srcAddr = srcAddr
	c.tcpGen = gen
	c.mu.Unlock()

	if oldTCP != nil {
		_ = oldTCP.Close()
	}

	c.clk.setMaster(clkAddr, gen)
	c.pl.reset(gen, t.bufferMs)

	log.Printf("subscribe master=%s src=%s clk=%s transport=%s buffer=%dms",
		t.master, srcAddr, clkAddr, t.transport, t.bufferMs)

	if t.transport == "tcp" {
		go c.dialTCP(srcAddr, gen)
	} else {
		c.helloUDP(srcAddr, true)
	}
}

func (c *client) helloUDP(dst *net.UDPAddr, prime bool) {
	var flag byte
	if prime {
		flag = flagPrimeMe
	}
	gen := c.clk.genFor()
	_, _ = c.udp.WriteToUDP(encodeFrame(typeHello, gen, 0, 0, []byte{flag}), dst)
}

func (c *client) control(typ byte, prime bool) {
	c.mu.Lock()
	tcp := c.tcpConn
	dst := c.srcAddr
	transport := c.cur.transport
	gen := c.tcpGen
	c.mu.Unlock()
	if transport == "tcp" {
		if tcp != nil {
			var flag byte
			if prime {
				flag = flagPrimeMe
			}
			_ = writeTCPFrame(tcp, encodeFrame(typ, gen, 0, 0, []byte{flag}))
		}
		return
	}
	if dst != nil {
		var flag byte
		if prime {
			flag = flagPrimeMe
		}
		_, _ = c.udp.WriteToUDP(encodeFrame(typ, c.clk.genFor(), 0, 0, []byte{flag}), dst)
	}
}

// keepaliveLoop: fast retry until the first frame, then 5s keepalive.
func (c *client) keepaliveLoop(ctx context.Context) {
	// fast retries while no frame yet
	for i := 0; i < helloRetries; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(helloRetryInterval):
		}
		if c.pl.gotFrames() {
			break
		}
		c.mu.Lock()
		dst, transport := c.srcAddr, c.cur.transport
		c.mu.Unlock()
		if dst != nil && transport != "tcp" {
			c.helloUDP(dst, true)
		}
	}
	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.control(typeHello, false)
		}
	}
}

func (p *playout) gotFrames() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gotFrame
}

// watchdogLoop issues a RESTART on >2s starvation (recommended for robustness).
func (c *client) watchdogLoop(ctx context.Context) {
	t := time.NewTicker(watchdogTimeout / 2)
	defer t.Stop()
	restarted := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.pl.starved() {
				if !restarted {
					log.Printf("starved >2s: sending RESTART")
					c.control(typeRestart, true)
					restarted = true
				}
			} else {
				restarted = false
			}
		}
	}
}

// readUDP is the single UDP read loop: clock replies + audio (+ RECONFIG).
func (c *client) readUDP(ctx context.Context) {
	buf := make([]byte, 64*1024)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = c.udp.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := c.udp.ReadFromUDP(buf)
		t4 := monoNow()
		if err != nil {
			continue
		}
		if n < headerSize || buf[0] != magic {
			continue // not ours / malformed -> ignore
		}
		h, ok := parseHeader(buf[:n])
		if !ok {
			continue
		}
		payload := buf[headerSize:n]
		switch h.typ {
		case typeClockRsp:
			c.clk.onReply(h, payload, t4)
		case typeAudio:
			c.onAudio(h, payload)
		case typeReconfig:
			c.onReconfig(h, payload)
		case typeFEC:
			// optional; ignored (gaps play silence)
		default:
			// unknown type -> forward-compat ignore
		}
	}
}

// onAudio gen-filters then pushes to the playout, re-arming on a gen change.
func (c *client) onAudio(h header, payload []byte) {
	cur := c.clk.genFor()
	if h.gen != cur {
		// A higher gen means a new/replaced session: re-arm to it. (Lower = stale.)
		if h.gen > cur {
			c.mu.Lock()
			buf := c.cur.bufferMs
			c.mu.Unlock()
			c.clk.bumpGen(h.gen)
			c.pl.reset(h.gen, buf)
		} else {
			return
		}
	}
	if len(payload) != frameBytes {
		return // PCM-only: anything else is a codec we can't decode
	}
	c.pl.push(h.seq, h.pts, payload)
}

func (c *client) onReconfig(h header, payload []byte) {
	stop := len(payload) > 0 && payload[0]&flagStop != 0
	log.Printf("RECONFIG gen=%d stop=%v", h.gen, stop)
	if stop {
		c.pl.reset(h.gen, c.bufferMs) // drop playback; await next session
		return
	}
	// Non-stop: resubscribe under the new gen. In --node mode the next poll
	// re-resolves settings; re-prime now so we don't wait up to 5s.
	c.clk.bumpGen(h.gen)
	c.pl.reset(h.gen, c.bufferMs)
	c.control(typeHello, true)
}

func (c *clockFollower) bumpGen(gen uint32) {
	c.mu.Lock()
	c.gen = gen
	clear(c.pending)
	c.mu.Unlock()
}

func (c *client) statsLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			played, silence, late, buffered := c.pl.stats()
			off, synced := c.clk.offset()
			offMs := off / 1_000_000
			log.Printf("comp=dumbclient played=%d silence=%d late=%d buffered=%d synced=%v offsetMs=%d",
				played, silence, late, buffered, synced, offMs)
		}
	}
}

func (c *clockFollower) loop(ctx context.Context) {
	c.probe()
	t := time.NewTicker(clockInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.probe()
		}
	}
}

// ---- TCP transport ---------------------------------------------------------

func (c *client) dialTCP(src *net.UDPAddr, gen uint32) {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.Dial("tcp", net.JoinHostPort(src.IP.String(), fmt.Sprintf("%d", src.Port)))
	if err != nil {
		log.Printf("tcp dial %s: %v", src, err)
		return
	}
	if err := writeTCPFrame(conn, encodeFrame(typeHello, gen, 0, 0, []byte{flagPrimeMe})); err != nil {
		_ = conn.Close()
		return
	}
	c.mu.Lock()
	c.tcpConn = conn
	c.mu.Unlock()
	c.readTCP(conn)
}

func (c *client) readTCP(conn net.Conn) {
	for {
		chunk, err := readTCPFrame(conn)
		if err != nil {
			return
		}
		if len(chunk) < headerSize || chunk[0] != magic {
			continue
		}
		h, ok := parseHeader(chunk)
		if !ok {
			continue
		}
		payload := chunk[headerSize:]
		switch h.typ {
		case typeAudio:
			c.onAudio(h, payload)
		case typeReconfig:
			c.onReconfig(h, payload)
		}
	}
}

// TCP length framing: uint32-BE length | chunk (D13).
func writeTCPFrame(w io.Writer, chunk []byte) error {
	var lp [4]byte
	binary.BigEndian.PutUint32(lp[:], uint32(len(chunk)))
	if _, err := w.Write(lp[:]); err != nil {
		return err
	}
	_, err := w.Write(chunk)
	return err
}

func readTCPFrame(r io.Reader) ([]byte, error) {
	var lp [4]byte
	if _, err := io.ReadFull(r, lp[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lp[:])
	if n < headerSize || n > 64*1024 {
		return nil, errors.New("bad frame length")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// ---- helpers ---------------------------------------------------------------

func waitSignal(cancel context.CancelFunc) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	cancel()
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
