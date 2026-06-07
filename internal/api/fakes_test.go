package api

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// fakeCluster implements Cluster for tests.
type fakeCluster struct {
	mu       sync.Mutex
	self     id.ID
	snap     contracts.Snapshot
	ch       chan struct{}
	dial     map[id.ID][]netip.Addr
	observed []observeCall

	setName     []string
	setVolume   []float64
	setDelay    []int
	setDevice   []string
	setDisabled [][]string
}

type observeCall struct {
	peer id.ID
	ip   netip.Addr
}

func newFakeCluster(self id.ID) *fakeCluster {
	return &fakeCluster{
		self: self,
		ch:   make(chan struct{}, 1),
		dial: map[id.ID][]netip.Addr{},
	}
}

func (f *fakeCluster) Self() id.ID { return f.self }

func (f *fakeCluster) Snapshot() contracts.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeCluster) setSnapshot(s contracts.Snapshot) {
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

func (f *fakeCluster) SetName(n string) {
	f.mu.Lock()
	f.setName = append(f.setName, n)
	f.mu.Unlock()
}

func (f *fakeCluster) SetVolume(v float64) {
	f.mu.Lock()
	f.setVolume = append(f.setVolume, v)
	f.mu.Unlock()
}

func (f *fakeCluster) SetOutputDelayMs(ms int) {
	f.mu.Lock()
	f.setDelay = append(f.setDelay, ms)
	f.mu.Unlock()
}

func (f *fakeCluster) SetOutputDevice(d string) {
	f.mu.Lock()
	f.setDevice = append(f.setDevice, d)
	f.mu.Unlock()
}

func (f *fakeCluster) SetDisabled(d []string) {
	f.mu.Lock()
	f.setDisabled = append(f.setDisabled, append([]string(nil), d...))
	f.mu.Unlock()
}

func (f *fakeCluster) Observe(peer id.ID, ip netip.Addr) {
	f.mu.Lock()
	f.observed = append(f.observed, observeCall{peer, ip})
	f.mu.Unlock()
}

func (f *fakeCluster) DialCandidates(peer id.ID) []netip.Addr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dial[peer]
}

func (f *fakeCluster) observeCalls() []observeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]observeCall(nil), f.observed...)
}

// fakeGroup implements Group for tests, recording calls and returning canned
// errors.
type fakeGroup struct {
	mu sync.Mutex

	followErr      error
	followTarget   id.ID
	unfollowErr    error
	unfollowN      int
	makeMasterErr  error
	makeMasterArg  id.ID
	nameErr        error
	nameGroup      id.ID
	nameName       string
	playErr        error
	playURI        string
	stopErr        error
	stopN          int
	pauseErr       error
	pauseN         int
	resumeErr      error
	resumeN        int
	settings       contracts.GroupSettings
	setSettingsErr error
	setSettingsArg contracts.GroupSettings
}

func (g *fakeGroup) Follow(_ context.Context, target id.ID) error {
	g.mu.Lock()
	g.followTarget = target
	g.mu.Unlock()
	return g.followErr
}

func (g *fakeGroup) Unfollow(context.Context) error {
	g.mu.Lock()
	g.unfollowN++
	g.mu.Unlock()
	return g.unfollowErr
}

func (g *fakeGroup) MakeMaster(_ context.Context, node id.ID) error {
	g.mu.Lock()
	g.makeMasterArg = node
	g.mu.Unlock()
	return g.makeMasterErr
}

func (g *fakeGroup) NameGroup(_ context.Context, group id.ID, name string) error {
	g.mu.Lock()
	g.nameGroup = group
	g.nameName = name
	g.mu.Unlock()
	return g.nameErr
}

func (g *fakeGroup) Play(_ context.Context, uri string) error {
	g.mu.Lock()
	g.playURI = uri
	g.mu.Unlock()
	return g.playErr
}

func (g *fakeGroup) Stop(context.Context) error {
	g.mu.Lock()
	g.stopN++
	g.mu.Unlock()
	return g.stopErr
}

func (g *fakeGroup) Pause(context.Context) error {
	g.mu.Lock()
	g.pauseN++
	g.mu.Unlock()
	return g.pauseErr
}

func (g *fakeGroup) Resume(context.Context) error {
	g.mu.Lock()
	g.resumeN++
	g.mu.Unlock()
	return g.resumeErr
}

func (g *fakeGroup) Settings() contracts.GroupSettings {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.settings
}

func (g *fakeGroup) SetSettings(_ context.Context, s contracts.GroupSettings) error {
	g.mu.Lock()
	g.setSettingsArg = s
	g.mu.Unlock()
	return g.setSettingsErr
}

// fakeNodeConfig implements NodeConfig.
type fakeNodeConfig struct {
	mu          sync.Mutex
	renameErr   error
	volErr      error
	delayErr    error
	deviceErr   error
	disabledErr error
	names       []string
	vols        []float64
	delays      []int
	devices     []string
	disabled    [][]string
}

func (n *fakeNodeConfig) Rename(name string) error {
	n.mu.Lock()
	n.names = append(n.names, name)
	n.mu.Unlock()
	return n.renameErr
}

func (n *fakeNodeConfig) SetVolume(v float64) error {
	n.mu.Lock()
	n.vols = append(n.vols, v)
	n.mu.Unlock()
	return n.volErr
}

func (n *fakeNodeConfig) SetOutputDelayMs(ms int) error {
	n.mu.Lock()
	n.delays = append(n.delays, ms)
	n.mu.Unlock()
	return n.delayErr
}

func (n *fakeNodeConfig) SetOutputDevice(d string) error {
	n.mu.Lock()
	n.devices = append(n.devices, d)
	n.mu.Unlock()
	return n.deviceErr
}

func (n *fakeNodeConfig) SetDisabled(d []string) error {
	n.mu.Lock()
	n.disabled = append(n.disabled, append([]string(nil), d...))
	n.mu.Unlock()
	return n.disabledErr
}

// fakeSink implements SinkControl.
type fakeSink struct {
	mu     sync.Mutex
	gains  []float64
	delays []int64
	tones  int
}

func (s *fakeSink) TestTone(time.Duration) error {
	s.mu.Lock()
	s.tones++
	s.mu.Unlock()
	return nil
}

func (s *fakeSink) SetGain(g float64) {
	s.mu.Lock()
	s.gains = append(s.gains, g)
	s.mu.Unlock()
}

func (s *fakeSink) SetDelayOffset(nanos int64) {
	s.mu.Lock()
	s.delays = append(s.delays, nanos)
	s.mu.Unlock()
}

// fakeMedia implements Media.
type fakeMedia struct {
	files []MediaFile
	err   error
}

func (m *fakeMedia) List() ([]MediaFile, error) { return m.files, m.err }
