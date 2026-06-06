package state

// cloneConfigDoc returns a deep copy of d so a caller that mutates a Get()
// result (or a snapshot handed to save) can never race the store (07 §5.1).
// Scalars and value structs (Cluster, Secrets, Auth scalars + Argon, per-group
// Media/Profile) are copied by assignment; every slice is copied element-wise.
// The nil-vs-empty distinction is preserved (clone only when != nil) so JSON
// round-trips are byte-stable.
func cloneConfigDoc(d ConfigDoc) ConfigDoc {
	out := d // copies all scalars + value structs; slices are shared until re-made below

	out.Auth.APIKeys = cloneStructSlice(d.Auth.APIKeys)

	if d.Nodes != nil {
		out.Nodes = make([]NodeRecord, len(d.Nodes))
		for i, n := range d.Nodes {
			cn := n // value copy (Caps scalars come along)
			cn.Addrs = cloneStrings(n.Addrs)
			cn.Caps.Sinks = cloneStrings(n.Caps.Sinks)
			cn.Caps.EncodeCodecs = cloneStrings(n.Caps.EncodeCodecs)
			cn.Caps.DecodeCodecs = cloneStrings(n.Caps.DecodeCodecs)
			cn.Caps.FEC = cloneStrings(n.Caps.FEC)
			out.Nodes[i] = cn
		}
	}

	if d.Groups != nil {
		out.Groups = make([]GroupRecord, len(d.Groups))
		for i, g := range d.Groups {
			cg := g // value copy (Media, Profile are value structs)
			cg.MemberNodeIDs = cloneStrings(g.MemberNodeIDs)
			out.Groups[i] = cg
		}
	}

	out.Revoked.Entries = cloneStructSlice(d.Revoked.Entries)
	return out
}

// cloneStrings deep-copies a []string, preserving nil.
func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// cloneStructSlice deep-copies a slice of value structs (no inner pointers/
// slices), preserving nil.
func cloneStructSlice[T any](s []T) []T {
	if s == nil {
		return nil
	}
	out := make([]T, len(s))
	copy(out, s)
	return out
}

// unionRevoked returns the grow-only union of two RevokedSets, deduplicated by
// Fingerprint (07 §4.3). Insertion order is resident (a) first, then incoming
// (b); a fingerprint present in both keeps a's RevokedCert value (resident wins
// metadata — the fingerprint is the identity). Never removes entries. The
// result is a fresh slice (never aliases a or b), and is nil only when both
// inputs contribute nothing.
func unionRevoked(a, b RevokedSet) RevokedSet {
	seen := make(map[string]struct{}, len(a.Entries)+len(b.Entries))
	out := make([]RevokedCert, 0, len(a.Entries)+len(b.Entries))
	for _, src := range [2][]RevokedCert{a.Entries, b.Entries} {
		for _, rc := range src {
			if _, ok := seen[rc.Fingerprint]; ok {
				continue
			}
			seen[rc.Fingerprint] = struct{}{}
			out = append(out, rc)
		}
	}
	if len(out) == 0 {
		return RevokedSet{}
	}
	return RevokedSet{Entries: out}
}
