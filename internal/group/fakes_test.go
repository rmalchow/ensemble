package group

import (
	"context"
	"io"
	"net/netip"
	"sync"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
	"ensemble/internal/stream"
)

// --- fakeCluster -------------------------------------------------------------

type fakeCluster struct {
	mu   sync.Mutex
	self id.ID
	snap contracts.Snapshot
	ch   chan struct{}

	following   []id.ID
	playback    []playbackCall
	settings    []settingsCall
	dialResults map[id.ID][]netip.Addr
}

type playbackCall struct {
	group id.ID
	pb    contracts.Playback
}
type settingsCall struct {
	group id.ID
	s     contracts.GroupSettings
}

func newFakeCluster(self id.ID) *fakeCluster {
	return &fakeCluster{
		self:        self,
		ch:          make(chan struct{}, 1),
		dialResults: map[id.ID][]netip.Addr{},
	}
}

func (f *fakeCluster) Self() id.ID { return f.self }

func (f *fakeCluster) Snapshot() contracts.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeCluster) setSnap(s contracts.Snapshot) {
	f.mu.Lock()
	f.snap = s
	f.mu.Unlock()
}

func (f *fakeCluster) Subscribe() <-chan struct{} { return f.ch }

func (f *fakeCluster) signal() {
	select {
	case f.ch <- struct{}{}:
	default:
	}
}

func (f *fakeCluster) SetFollowing(t id.ID) {
	f.mu.Lock()
	f.following = append(f.following, t)
	f.mu.Unlock()
}
func (f *fakeCluster) lastFollowing() (id.ID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.following) == 0 {
		return id.Zero, false
	}
	return f.following[len(f.following)-1], true
}
func (f *fakeCluster) followCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.following)
}

func (f *fakeCluster) SetPlayback(g id.ID, pb contracts.Playback) {
	f.mu.Lock()
	f.playback = append(f.playback, playbackCall{g, pb})
	f.mu.Unlock()
}
func (f *fakeCluster) lastPlayback() (playbackCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.playback) == 0 {
		return playbackCall{}, false
	}
	return f.playback[len(f.playback)-1], true
}

func (f *fakeCluster) SetGroupSettings(g id.ID, s contracts.GroupSettings) {
	f.mu.Lock()
	f.settings = append(f.settings, settingsCall{g, s})
	f.mu.Unlock()
}
func (f *fakeCluster) lastSettings() (settingsCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.settings) == 0 {
		return settingsCall{}, false
	}
	return f.settings[len(f.settings)-1], true
}

func (f *fakeCluster) DialCandidates(peer id.ID) []netip.Addr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dialResults[peer]
}

// --- fakeFollowClient --------------------------------------------------------

type followCall struct {
	peer, target id.ID
	unfollow     bool
}

type fakeFollowClient struct {
	mu    sync.Mutex
	calls []followCall
	errFn func(peer id.ID) error
}

func (f *fakeFollowClient) Follow(_ context.Context, peer, target id.ID) error {
	f.mu.Lock()
	f.calls = append(f.calls, followCall{peer: peer, target: target})
	f.mu.Unlock()
	if f.errFn != nil {
		return f.errFn(peer)
	}
	return nil
}
func (f *fakeFollowClient) Unfollow(_ context.Context, peer id.ID) error {
	f.mu.Lock()
	f.calls = append(f.calls, followCall{peer: peer, unfollow: true})
	f.mu.Unlock()
	if f.errFn != nil {
		return f.errFn(peer)
	}
	return nil
}
func (f *fakeFollowClient) snapshot() []followCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]followCall(nil), f.calls...)
}

// --- fakeMedia / fakeSource --------------------------------------------------

type fakeSource struct {
	mu        sync.Mutex
	remaining int // pull frames left before io.EOF (ignored when live)
	live      bool
	closed    bool
	readCount int
	pattern   byte // first byte written per frame, incremented each read
}

func (s *fakeSource) ReadFrame(dst []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return io.EOF
	}
	if !s.live {
		if s.remaining <= 0 {
			return io.EOF
		}
		s.remaining--
	}
	for i := range dst[:stream.FrameBytes] {
		dst[i] = 0
	}
	dst[0] = s.pattern
	s.pattern++
	s.readCount++
	return nil
}
func (s *fakeSource) Live() bool { return s.live }
func (s *fakeSource) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}
func (s *fakeSource) reads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readCount
}

type fakeMedia struct {
	src *fakeSource
	err error
	uri string
}

func (m *fakeMedia) Open(uri string) (MediaSource, error) {
	m.uri = uri
	if m.err != nil {
		return nil, m.err
	}
	return m.src, nil
}

// --- fakeSourceServer --------------------------------------------------------

type releaseCall struct {
	pts     int64
	payload []byte
}

type fakeSourceServer struct {
	mu        sync.Mutex
	starts    []startCall
	releases  []releaseCall
	reconfigs int
	stops     int
	stats     contracts.SourceStats
}

type startCall struct {
	gen      uint32
	t        stream.Transport
	bufferMs int
}

func (s *fakeSourceServer) StartSession(gen uint32, t stream.Transport, bufferMs int) {
	s.mu.Lock()
	s.starts = append(s.starts, startCall{gen, t, bufferMs})
	s.mu.Unlock()
}
func (s *fakeSourceServer) ReleaseFrame(pts int64, payload []byte) uint64 {
	s.mu.Lock()
	seq := uint64(len(s.releases))
	s.releases = append(s.releases, releaseCall{pts, append([]byte(nil), payload...)})
	s.mu.Unlock()
	return seq
}
func (s *fakeSourceServer) Reconfig() {
	s.mu.Lock()
	s.reconfigs++
	s.mu.Unlock()
}
func (s *fakeSourceServer) StopSession() {
	s.mu.Lock()
	s.stops++
	s.mu.Unlock()
}
func (s *fakeSourceServer) Stats() contracts.SourceStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}
func (s *fakeSourceServer) snapshotReleases() []releaseCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]releaseCall(nil), s.releases...)
}
func (s *fakeSourceServer) startCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.starts)
}
func (s *fakeSourceServer) lastStart() (startCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.starts) == 0 {
		return startCall{}, false
	}
	return s.starts[len(s.starts)-1], true
}
func (s *fakeSourceServer) stopCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stops
}

// --- fakeSubscriber ----------------------------------------------------------

type subCall struct {
	addr netip.AddrPort
	gen  uint32
	t    stream.Transport
}

type fakeSubscriber struct {
	mu     sync.Mutex
	subs   []subCall
	unsubs int
}

func (s *fakeSubscriber) Subscribe(addr netip.AddrPort, gen uint32, t stream.Transport) error {
	s.mu.Lock()
	s.subs = append(s.subs, subCall{addr, gen, t})
	s.mu.Unlock()
	return nil
}
func (s *fakeSubscriber) Unsubscribe() {
	s.mu.Lock()
	s.unsubs++
	s.mu.Unlock()
}
func (s *fakeSubscriber) snapshotSubs() []subCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]subCall(nil), s.subs...)
}

// --- fakeSink ----------------------------------------------------------------

type fakeSink struct {
	mu      sync.Mutex
	resets  []uint32
	disarms int
}

func (s *fakeSink) Push(uint32, uint64, int64, []byte) {}
func (s *fakeSink) Reset(gen uint32) {
	s.mu.Lock()
	s.resets = append(s.resets, gen)
	s.mu.Unlock()
}
func (s *fakeSink) Disarm() {
	s.mu.Lock()
	s.disarms++
	s.mu.Unlock()
}
func (s *fakeSink) Stats() contracts.SinkStats { return contracts.SinkStats{} }
func (s *fakeSink) SetGain(float64)            {}
func (s *fakeSink) SetDelayOffset(int64)       {}
func (s *fakeSink) Close() error               { return nil }
func (s *fakeSink) snapshotResets() []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint32(nil), s.resets...)
}
func (s *fakeSink) disarmCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disarms
}

// --- fakeClock ---------------------------------------------------------------

type fakeClock struct {
	mu     sync.Mutex
	offset int64
	ok     bool
}

func (c *fakeClock) setOK(ok bool) {
	c.mu.Lock()
	c.ok = ok
	c.mu.Unlock()
}
func (c *fakeClock) MasterNow() (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset, c.ok
}
func (c *fakeClock) MasterToLocal(m int64) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return m - c.offset, c.ok
}
func (c *fakeClock) LocalToMaster(l int64) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return l + c.offset, c.ok
}

// --- fakeClockCtl ------------------------------------------------------------

type clockCtlCall struct {
	dst netip.AddrPort
	gen uint32
}

type fakeClockCtl struct {
	mu    sync.Mutex
	calls []clockCtlCall
}

func (c *fakeClockCtl) SetMaster(dst netip.AddrPort, gen uint32) {
	c.mu.Lock()
	c.calls = append(c.calls, clockCtlCall{dst, gen})
	c.mu.Unlock()
}
func (c *fakeClockCtl) snapshot() []clockCtlCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]clockCtlCall(nil), c.calls...)
}

// --- fakeOpus ----------------------------------------------------------------

type fakeOpusEncoder struct {
	closed bool
	buf    []byte
}

func (e *fakeOpusEncoder) Encode(pcm []byte) ([]byte, error) {
	// Trivial "encode": first 8 bytes, aliasing a reused buffer (to exercise the
	// copy-before-fanout contract).
	if e.buf == nil {
		e.buf = make([]byte, 8)
	}
	copy(e.buf, pcm)
	return e.buf, nil
}
func (e *fakeOpusEncoder) Close() error { e.closed = true; return nil }

type fakeOpusFactory struct {
	err error
	enc *fakeOpusEncoder
}

func (f *fakeOpusFactory) NewEncoder() (OpusEncoder, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.enc == nil {
		f.enc = &fakeOpusEncoder{}
	}
	return f.enc, nil
}
