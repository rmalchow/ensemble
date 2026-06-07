// Pure formatters (J arch §3.4). No state, no DOM.

// shortId returns the first 8 hex chars of an id (matches the default node
// name, spec §1). Tolerates shorter input.
export function shortId(id) {
  if (!id) return "";
  return String(id).slice(0, 8);
}

// relTime renders a unix-seconds timestamp as a coarse relative string.
// Accepts a number (unix seconds) or an RFC3339/parseable date string.
export function relTime(unixSec) {
  if (!unixSec) return "never";
  let secs = unixSec;
  if (typeof secs !== "number") {
    const t = Date.parse(secs);
    if (Number.isNaN(t)) return "—";
    secs = t / 1000;
  }
  const now = Date.now() / 1000;
  let d = Math.floor(now - secs);
  if (d < 0) d = 0;
  if (d < 5) return "just now";
  if (d < 60) return d + "s ago";
  if (d < 3600) return Math.floor(d / 60) + "m ago";
  if (d < 86400) return Math.floor(d / 3600) + "h ago";
  return Math.floor(d / 86400) + "d ago";
}

// position formats a seconds count as m:ss (75.0 → "1:15").
export function position(sec) {
  if (!sec || sec < 0) sec = 0;
  const total = Math.floor(sec);
  const m = Math.floor(total / 60);
  const s = total % 60;
  return m + ":" + String(s).padStart(2, "0");
}

// bytes scales a byte count to B/KB/MB/GB with one decimal.
export function bytes(n) {
  if (!n || n < 0) n = 0;
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  const s = i === 0 ? String(v) : v.toFixed(1);
  return s + " " + units[i];
}

// cidrList joins CIDR strings; "—" when empty.
export function cidrList(addrs) {
  if (!addrs || addrs.length === 0) return "—";
  return addrs.join(", ");
}

// ports renders a NodeView's port set compactly, omitting unset ports:
// "http 8080 · stream 9090 · source 9200 · gossip 7946".
export function ports(node) {
  if (!node) return "";
  const parts = [];
  if (node.httpPort) parts.push("http " + node.httpPort);
  if (node.streamPort) parts.push("stream " + node.streamPort);
  if (node.sourcePort) parts.push("source " + node.sourcePort);
  if (node.gossipPort) parts.push("gossip " + node.gossipPort);
  return parts.join(" · ");
}
