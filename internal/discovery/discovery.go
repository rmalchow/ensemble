package discovery

import (
	"context"
	"net"
	"strconv"
	"strings"

	"github.com/grandcat/zeroconf"
)

// mdnsService is the DNS-SD service type advertised on the LAN, and mdnsDomain
// the mDNS domain. Discovery is used only to bootstrap memberlist: a node finds
// a peer or two, hands their gossip addresses to memberlist.Join, and from then
// on SWIM gossip is the source of truth for membership (doc 02 §2.1).
const (
	mdnsService = "_ensemble._udp" // doc 02 §2.2
	mdnsDomain  = "local."
)

// Discovery holds the live mDNS advertisement for this node (wraps a
// *zeroconf.Server). The advertised SRV/A-record port is the memberlist gossip
// port; all node metadata rides the TXT records (doc 02 §2.2).
type Discovery struct {
	server *zeroconf.Server
}

// Announce describes everything advertised over mDNS for this node. The
// memberlist gossip port is the SRV/A-record port, passed separately to
// Register; the nine fields below ride TXT (doc 02 §2.2 key table). An
// uninitialized node sets ClusterFP=="" and Initialized==false (the adoption
// hook, doc 02 §2.4).
type Announce struct {
	NodeID      string // TXT "id"
	Name        string // TXT "name"
	ClusterFP   string // TXT "cf"  — "" ⇒ uninitialized
	GroupID     string // TXT "gid"
	Initialized bool   // TXT "init" == "1"
	ControlPort int    // TXT "ctrl" — mTLS API (default 8443, A.12)
	ClockPort   int    // TXT "clk"  — clock UDP (default 9000, A.12)
	AudioPort   int    // TXT "aud"  — audio UDP (default 9100, A.12)
	WebPort     int    // TXT "wp"   — browser deep-link
}

// Register advertises this node over mDNS. gossipPort is the memberlist gossip
// port — the SRV/A-record port peers feed to memberlist.Join; everything else
// rides TXT. (cf. mpvsync discovery.go:31 Register.)
func Register(a Announce, gossipPort int) (*Discovery, error) {
	srv, err := zeroconf.Register(
		a.NodeID,
		mdnsService,
		mdnsDomain,
		gossipPort,
		txtRecords(a),
		nil, // all interfaces
	)
	if err != nil {
		return nil, err
	}
	return &Discovery{server: srv}, nil
}

// Close withdraws the mDNS advertisement.
func (d *Discovery) Close() {
	if d.server != nil {
		d.server.Shutdown()
	}
}

// Browse returns memberlist seed addresses ("ip:port", gossip port) for peers in
// OUR cluster — those advertising TXT cf == clusterFP — excluding selfID, for the
// duration of ctx. Its result feeds memberlist.Join. (mpvsync discovery.go:55
// Browse — rekeyed group→cf, doc 02 §2.3.)
func Browse(ctx context.Context, clusterFP, selfID string) ([]string, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}
	entries := make(chan *zeroconf.ServiceEntry, 16)
	if err := resolver.Browse(ctx, mdnsService, mdnsDomain, entries); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var seeds []string
	for {
		select {
		case <-ctx.Done():
			return seeds, nil
		case e, ok := <-entries:
			if !ok {
				return seeds, nil
			}
			txt := parseTXT(e.Text)
			if txt["cf"] != clusterFP || txt["id"] == selfID {
				continue
			}
			for _, ip := range e.AddrIPv4 {
				addr := net.JoinHostPort(ip.String(), strconv.Itoa(e.Port))
				if !seen[addr] {
					seen[addr] = true
					seeds = append(seeds, addr)
				}
			}
		}
	}
}

// DiscoveredNode is one mDNS-advertised peer (doc 02 §2.3, verbatim). Unlike
// Browse (which filters to our own cluster and returns seed addresses), BrowseAll
// surfaces every advertised node — foreign clusters and uninitialized nodes
// included — so the UI can classify and offer adoption/takeover.
type DiscoveredNode struct {
	NodeID      string
	Name        string
	ClusterFP   string // TXT "cf" — "" means uninitialized
	GroupID     string // TXT "gid"
	Initialized bool   // TXT "init" == "1"
	Addr        string // first IPv4
	Port        int    // memberlist gossip port
	ControlPort int    // TXT "ctrl" — mTLS API
	ClockPort   int    // TXT "clk"  — clock UDP
	AudioPort   int    // TXT "aud"  — audio UDP
	WebPort     int    // TXT "wp"
}

// BrowseAll surfaces EVERY advertised node (all clusters, uninitialized included)
// for the duration of ctx, dedup'd by id and selecting the first IPv4. The result
// drives the UI's 5-state classification and is meant to be CACHED by the caller
// on the 5 s rebrowse ticker — do NOT browse per WS tick (doc 02 §2.3, §3.5).
// (mpvsync discovery.go:106 BrowseAll.)
func BrowseAll(ctx context.Context) ([]DiscoveredNode, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}
	entries := make(chan *zeroconf.ServiceEntry, 16)
	if err := resolver.Browse(ctx, mdnsService, mdnsDomain, entries); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var nodes []DiscoveredNode
	for {
		select {
		case <-ctx.Done():
			return nodes, nil
		case e, ok := <-entries:
			if !ok {
				return nodes, nil
			}
			txt := parseTXT(e.Text)
			id := txt["id"]
			if id == "" || seen[id] {
				continue
			}
			var addr string
			if len(e.AddrIPv4) > 0 {
				addr = e.AddrIPv4[0].String()
			}
			seen[id] = true
			nodes = append(nodes, nodeFromTXT(txt, addr, e.Port))
		}
	}
}

// txtRecords builds the mDNS TXT key=value list this node advertises (doc 02 §2.2
// key set). init is "1"/"0"; ports are decimal strings; an uninitialized node
// sends cf="" and init=0.
func txtRecords(a Announce) []string {
	return []string{
		"id=" + a.NodeID,
		"name=" + a.Name,
		"cf=" + a.ClusterFP,
		"gid=" + a.GroupID,
		"init=" + boolTXT(a.Initialized),
		"ctrl=" + strconv.Itoa(a.ControlPort),
		"clk=" + strconv.Itoa(a.ClockPort),
		"aud=" + strconv.Itoa(a.AudioPort),
		"wp=" + strconv.Itoa(a.WebPort),
	}
}

// nodeFromTXT decodes a parsed TXT map (plus the resolved first-IPv4 addr and
// gossip port) into a DiscoveredNode. Absent or unparseable ports decode to 0.
func nodeFromTXT(txt map[string]string, addr string, gossipPort int) DiscoveredNode {
	return DiscoveredNode{
		NodeID:      txt["id"],
		Name:        txt["name"],
		ClusterFP:   txt["cf"],
		GroupID:     txt["gid"],
		Initialized: txt["init"] == "1",
		Addr:        addr,
		Port:        gossipPort,
		ControlPort: atoiTXT(txt["ctrl"]),
		ClockPort:   atoiTXT(txt["clk"]),
		AudioPort:   atoiTXT(txt["aud"]),
		WebPort:     atoiTXT(txt["wp"]),
	}
}

// boolTXT maps the init flag to its "1"/"0" TXT value.
func boolTXT(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// atoiTXT parses a decimal TXT port value, returning 0 for an empty or malformed
// value (robust parse — a peer with no port advertised decodes to 0).
func atoiTXT(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// parseTXT splits a "key=value" TXT slice into a map.
func parseTXT(txt []string) map[string]string {
	out := make(map[string]string, len(txt))
	for _, kv := range txt {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}
