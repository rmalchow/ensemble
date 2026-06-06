// Package cluster owns LAN membership (memberlist SWIM gossip), the live peer
// set, the previously-seen peer cache, and per-group master election.
//
//   - Membership/failure detection uses hashicorp/memberlist (SWIM gossip),
//     with the LAN preset timings pinned by A.14.2.
//   - Each node advertises a small Meta blob (its id, name, group, cluster CA
//     fingerprint, and the control/clock/audio/web service ports) so peers can
//     locate the elected master's endpoints (02 §3.1).
//   - The gossip delegate (delegate.go) bridges the replicated ConfigDoc into
//     memberlist push/pull anti-entropy (A.14.2): it calls into internal/state
//     (MarshalGossip/MergeGossip). cluster imports state, never the reverse —
//     the gossip seam is inverted so state stays a near-leaf (01 §2.2).
//   - Election (election.go / elections.go) deterministically picks one master
//     per group via the soft MasterHint + lowest-stable-id rule (A.5, 02 §5).
//   - PeerStore (peers.go) persists seen gossip seeds to peers.json for fast
//     rejoin (A.14.1).
//
// Layering (01 §2): cluster may import internal/state only; it must never reach
// up into group/stream/audio/web/discovery/clock/pki/auth/allowlist. cmd wires
// discovery results into Join and feeds Members()/Changed()/Recompute() to the
// downstream consumers.
package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// Meta is the per-node metadata gossiped via memberlist Node.Meta (02 §3.1).
// The JSON keys are short to stay well under memberlist's NodeMeta size limit.
type Meta struct {
	NodeID      string `json:"id"`
	Name        string `json:"name"`
	GroupID     string `json:"gid"`
	ClusterFP   string `json:"cf"`   // cluster CA fingerprint; "" => uninitialized
	ControlPort int    `json:"ctrl"` // mTLS HTTP API (A.12: 8443)
	ClockPort   int    `json:"clk"`  // clock UDP (A.12: 9000)
	AudioPort   int    `json:"aud"`  // audio UDP (A.12: 9100)
	WebPort     int    `json:"wp"`
}

// Member is an alive peer in the cluster (self included).
type Member struct {
	Addr net.IP
	Port uint16 // memberlist gossip port (sourced from the live memberlist Node)
	Meta Meta
}

// GossipAddr returns the member's memberlist gossip address (host:port), the
// form memberlist.Join accepts. It is sourced from the live memberlist node's
// Addr+Port, so it round-trips through the previously-seen peer cache for a
// faster rejoin on restart.
func (m Member) GossipAddr() string {
	return net.JoinHostPort(m.Addr.String(), itoa(int(m.Port)))
}

// ControlAddr returns the member's mTLS control-API address (host:port).
func (m Member) ControlAddr() string {
	return net.JoinHostPort(m.Addr.String(), itoa(m.Meta.ControlPort))
}

// ClockAddr returns the member's clock-server address (host:port).
func (m Member) ClockAddr() string {
	return net.JoinHostPort(m.Addr.String(), itoa(m.Meta.ClockPort))
}

// AudioAddr returns the member's audio-transport address (host:port).
func (m Member) AudioAddr() string {
	return net.JoinHostPort(m.Addr.String(), itoa(m.Meta.AudioPort))
}

// WebAddr returns the member's web UI address (host:port).
func (m Member) WebAddr() string {
	return net.JoinHostPort(m.Addr.String(), itoa(m.Meta.WebPort))
}

// Config configures a Membership.
type Config struct {
	NodeID      string
	Name        string
	GroupID     string
	ClusterFP   string
	BindAddr    string // default "0.0.0.0"
	BindPort    int    // memberlist gossip port (required) — A.12: 7946
	ControlPort int    // A.12: 8443
	ClockPort   int    // A.12: 9000
	AudioPort   int    // A.12: 9100
	WebPort     int

	// SecretKey, when non-empty, enables memberlist gossip encryption keyed by
	// it. It is fed the cluster gossipKey (32 random bytes from ClusterSecrets),
	// NOT an HKDF-of-password (02 §3.2). Empty => open/unencrypted gossip.
	SecretKey []byte
	Seeds     []string  // explicit join addresses (host or host:port)
	LogOutput io.Writer // memberlist logs; nil => io.Discard

	// State, if set, is the shared ConfigDoc store bridged into memberlist
	// push/pull anti-entropy (A.14.2) via the delegate. cluster calls into
	// state; the dependency never runs the other way.
	State *state.Store
}

// Membership wraps a memberlist cluster for this node.
type Membership struct {
	ml       *memberlist.Memberlist
	changed  chan struct{}
	keyring  *memberlist.Keyring // nil when started without a SecretKey (open)
	bindPort int

	// del is this node's delegate; its meta bytes are re-marshaled and
	// re-broadcast (memberlist UpdateNode) when the friendly name changes so
	// peers see the rename. self/del meta are guarded by metaMu.
	del    *delegate
	self   Meta
	metaMu sync.Mutex
}

// New creates and starts a memberlist node, joining any provided seeds. The
// base profile is memberlist.DefaultLANConfig() (A.14.2: GossipInterval 200ms,
// ProbeInterval 1s, ProbeTimeout 500ms — already the LAN preset, left
// untouched); only Name/Bind*/Advertise*/Delegate/Events/SecretKey/LogOutput
// are overridden to avoid drift from A.12/A.14.2.
func New(cfg Config) (*Membership, error) {
	if cfg.BindAddr == "" {
		cfg.BindAddr = "0.0.0.0"
	}
	self := Meta{
		NodeID:      cfg.NodeID,
		Name:        cfg.Name,
		GroupID:     cfg.GroupID,
		ClusterFP:   cfg.ClusterFP,
		ControlPort: cfg.ControlPort,
		ClockPort:   cfg.ClockPort,
		AudioPort:   cfg.AudioPort,
		WebPort:     cfg.WebPort,
	}
	metaBytes, err := json.Marshal(self)
	if err != nil {
		return nil, err
	}

	evCh := make(chan memberlist.NodeEvent, 64)
	del := &delegate{meta: metaBytes, state: cfg.State}
	m := &Membership{
		self:     self,
		changed:  make(chan struct{}, 1),
		bindPort: cfg.BindPort,
		del:      del,
	}

	mlc := memberlist.DefaultLANConfig()
	mlc.Name = cfg.NodeID // must be unique; the stable node id guarantees that (02 §3.1)
	mlc.BindAddr = cfg.BindAddr
	mlc.BindPort = cfg.BindPort
	mlc.AdvertisePort = cfg.BindPort
	mlc.Delegate = del
	mlc.Events = &memberlist.ChannelEventDelegate{Ch: evCh}
	if cfg.LogOutput != nil {
		mlc.LogOutput = cfg.LogOutput
	} else {
		mlc.LogOutput = io.Discard
	}
	if len(cfg.SecretKey) > 0 {
		mlc.SecretKey = cfg.SecretKey
	}

	ml, err := memberlist.Create(mlc)
	if err != nil {
		return nil, fmt.Errorf("memberlist create: %w", err)
	}
	m.ml = ml
	// memberlist auto-initializes a Keyring from SecretKey; retain it so the node
	// can re-key at runtime (only possible when it started with a key — an open
	// node must restart to enable encryption; see Rekey).
	m.keyring = mlc.Keyring

	// Coalesce raw membership events into a single "something changed" signal
	// (A.14.2): callers re-read Members() on each receipt rather than per event.
	go func() {
		for range evCh {
			m.signal()
		}
	}()

	if len(cfg.Seeds) > 0 {
		// Join is non-fatal: we may be the first node, or seeds may be offline;
		// discovery/anti-entropy reconciles as peers appear.
		_, _ = ml.Join(cfg.Seeds)
	}
	return m, nil
}

// Join attempts to join additional seed addresses (e.g. from discovery).
func (m *Membership) Join(addrs []string) (int, error) { return m.ml.Join(addrs) }

// BindPort returns the memberlist gossip port (advertised so adopters can Join).
func (m *Membership) BindPort() int { return m.bindPort }

// Rekey installs key as the new primary gossip encryption key at runtime,
// keeping any prior key on the ring so in-flight messages still decrypt. It
// returns an error if this node was started without encryption (no keyring): an
// open→encrypted transition requires a coordinated restart (the SecretKey is
// set at memberlist creation). A nil/empty key is a no-op.
func (m *Membership) Rekey(key []byte) error {
	if len(key) == 0 {
		return nil
	}
	if m.keyring == nil {
		return fmt.Errorf("cannot enable encryption at runtime; restart with the cluster gossip key set")
	}
	if err := m.keyring.AddKey(key); err != nil {
		return err
	}
	return m.keyring.UseKey(key)
}

// Members returns the current alive members (including self) with decoded Meta.
// Meta is re-decoded on each call; callers re-read on Changed()/the 2s safety
// tick, not per packet (02 §3.5).
func (m *Membership) Members() []Member {
	nodes := m.ml.Members()
	out := make([]Member, 0, len(nodes))
	for _, n := range nodes {
		meta, err := decodeMeta(n.Meta)
		if err != nil {
			continue
		}
		out = append(out, Member{Addr: n.Addr, Port: n.Port, Meta: meta})
	}
	return out
}

// Self returns this node's metadata.
func (m *Membership) Self() Meta {
	m.metaMu.Lock()
	defer m.metaMu.Unlock()
	return m.self
}

// UpdateName changes this node's advertised friendly name: it updates the source
// Meta, re-marshals the delegate's NodeMeta bytes, and calls memberlist
// UpdateNode so the change is re-broadcast and peers see it within a couple of
// seconds. A no-op if the name is unchanged or marshaling fails.
func (m *Membership) UpdateName(name string) error {
	m.metaMu.Lock()
	if m.self.Name == name {
		m.metaMu.Unlock()
		return nil
	}
	m.self.Name = name
	updated := m.self
	b, err := json.Marshal(updated)
	if err != nil {
		m.metaMu.Unlock()
		return err
	}
	if m.del != nil {
		m.del.setMeta(b)
	}
	m.metaMu.Unlock()
	// UpdateNode re-invokes NodeMeta and gossips the refreshed meta to peers.
	return m.ml.UpdateNode(5 * time.Second)
}

// Changed returns a channel that signals when membership changes. It is
// coalesced (at most one pending signal), so re-read Members() on each receipt.
func (m *Membership) Changed() <-chan struct{} { return m.changed }

// NumMembers returns the count of alive members.
func (m *Membership) NumMembers() int { return m.ml.NumMembers() }

// Leave gracefully announces departure and shuts the node down.
func (m *Membership) Leave() error {
	_ = m.ml.Leave(0)
	return m.ml.Shutdown()
}

func (m *Membership) signal() {
	select {
	case m.changed <- struct{}{}:
	default:
	}
}

func decodeMeta(b []byte) (Meta, error) {
	if len(b) == 0 {
		return Meta{}, fmt.Errorf("empty meta")
	}
	var meta Meta
	err := json.Unmarshal(b, &meta)
	return meta, err
}

func itoa(i int) string { return strconv.Itoa(i) }
