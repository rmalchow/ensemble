package discovery

import "sort"

// NodeState is the UI device classification (doc 02 §2.4, table). It extends
// mpvsync's 4-state buildDevices with the uninitialized state so the UI can offer
// adoption.
type NodeState string

const (
	StatePresent       NodeState = "present"       // same cluster, alive gossip peer
	StateOffline       NodeState = "offline"       // known member, not currently alive
	StateUninitialized NodeState = "uninitialized" // init=0 / cf=="" ⇒ "Adopt"
	StateForeign       NodeState = "foreign"       // cf != "" and cf != ourCF ⇒ "Takeover"
	StateDiscovered    NodeState = "discovered"    // same cf, advertised, not yet a gossip member
)

// ClassifyInput is the read-only view Classify needs. The caller (cmd/web)
// supplies these sets from cluster.Members() (alive), the ConfigDoc/peer-cache
// (known members), and BrowseAll() (the mDNS survey). Discovery owns the RULE,
// not the data sources — which keeps the package leaf-level and independently
// testable (doc 02 §6, P2.2 §6 import constraints).
type ClassifyInput struct {
	OurClusterFP string           // "" if this node is itself uninitialized
	AliveIDs     map[string]bool  // ids alive in gossip (same cluster) — cluster.Members()
	KnownIDs     map[string]bool  // ids that are adopted members (ConfigDoc.Nodes ∪ peer cache)
	Discovered   []DiscoveredNode // BrowseAll() survey (cached)
}

// Device is one classified row for the UI: a node id, its chosen state, and the
// survey record if the node is currently advertising (nil for an offline member
// known from the ConfigDoc/peer cache but not seen in the survey).
type Device struct {
	NodeID string
	State  NodeState
	Node   *DiscoveredNode // nil for an offline known-but-not-advertised member
}

// Classify folds the alive/known/discovered inputs into exactly one row per node
// id, applying the precedence present > offline > {discovered|foreign|uninitialized}
// (mpvsync api_cluster.go:60). Output is sorted by node id for a stable UI order.
func Classify(in ClassifyInput) []Device {
	byID := make(map[string]*Device)

	// 1) Known members: present if also alive in gossip, else offline. These win
	//    over any survey row for the same id (the precedence rule).
	for id := range in.KnownIDs {
		if id == "" {
			continue
		}
		state := StateOffline
		if in.AliveIDs[id] {
			state = StatePresent
		}
		byID[id] = &Device{NodeID: id, State: state}
	}

	// 2) Survey rows for ids not already resolved to present/offline. A node may be
	//    alive in gossip before it lands in KnownIDs (freshly joined), so an alive
	//    id is treated as present here too — never downgraded to discovered.
	for i := range in.Discovered {
		d := &in.Discovered[i]
		if d.NodeID == "" || byID[d.NodeID] != nil {
			continue
		}
		var state NodeState
		switch {
		case in.AliveIDs[d.NodeID]:
			state = StatePresent
		case !d.Initialized || d.ClusterFP == "":
			state = StateUninitialized
		case d.ClusterFP != in.OurClusterFP:
			state = StateForeign
		default: // d.ClusterFP == in.OurClusterFP
			state = StateDiscovered
		}
		byID[d.NodeID] = &Device{NodeID: d.NodeID, State: state, Node: d}
	}

	out := make([]Device, 0, len(byID))
	for _, dv := range byID {
		out = append(out, *dv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}
