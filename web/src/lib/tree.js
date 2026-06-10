// Pure directory-derivation over the FLAT recursive media list (J arch §4 Media).
// GET /api/media returns [{path, name, sizeBytes, modTime}] where path is
// slash-separated relative to the node's media dir (e.g. "albums/xyz/01.mp3").
// These helpers turn that flat list into a navigable directory view, entirely
// client-side (no API change). No state — pure functions, unit-tested.

// normDir trims leading/trailing slashes so "" / "/" / "albums/" all behave the
// same and join cleanly. The current-directory state uses the "" root form.
function normDir(dir) {
  return (dir || "").replace(/^\/+|\/+$/g, "");
}

// caseless compares two strings case-insensitively (stable, locale-independent).
function caseless(a, b) {
  const la = a.toLowerCase();
  const lb = b.toLowerCase();
  return la < lb ? -1 : la > lb ? 1 : 0;
}

// entriesFor derives the contents of a single directory from the flat file list:
//   { folders: [{name, count}], files: [MediaFile, …] }
// - folders: the unique next path segments under `dir` (immediate subdirs),
//   each with `count` = number of files contained anywhere beneath it.
// - files: the files that live directly in `dir` (no further slash).
// Both are sorted alphabetically, case-insensitive; folders precede files.
export function entriesFor(files, dir) {
  const base = normDir(dir);
  const prefix = base ? base + "/" : "";
  const folderCounts = new Map(); // segment name → contained-file count
  const here = [];

  for (const f of files || []) {
    if (!f || typeof f.path !== "string") continue;
    const path = f.path.replace(/^\/+/, "");
    if (prefix && !path.startsWith(prefix)) continue;
    const rest = path.slice(prefix.length);
    if (!rest) continue;
    const slash = rest.indexOf("/");
    if (slash === -1) {
      // a file directly in this directory.
      here.push(f);
    } else {
      // a file nested under a subfolder of this directory.
      const seg = rest.slice(0, slash);
      folderCounts.set(seg, (folderCounts.get(seg) || 0) + 1);
    }
  }

  const folders = [...folderCounts.entries()]
    .map(([name, count]) => ({ name, count }))
    .sort((a, b) => caseless(a.name, b.name));

  const sortedFiles = here
    .slice()
    .sort((a, b) => caseless(a.name || "", b.name || ""));

  return { folders, files: sortedFiles };
}

// filesUnder returns EVERY file at or beneath `dir` (recursive), sorted by full
// path — used to enqueue a whole folder subtree in a stable order.
export function filesUnder(files, dir) {
  const base = normDir(dir);
  const prefix = base ? base + "/" : "";
  return (files || [])
    .filter((f) => f && typeof f.path === "string")
    .filter((f) => {
      const path = f.path.replace(/^\/+/, "");
      return !prefix || path.startsWith(prefix);
    })
    .slice()
    .sort((a, b) => caseless(a.path, b.path));
}

// crumbs turns a directory path into breadcrumb segments for navigation:
//   crumbs("albums/xyz") → [
//     { name: "media", dir: "" },
//     { name: "albums", dir: "albums" },
//     { name: "xyz",    dir: "albums/xyz" },
//   ]
// The first crumb ("media") is always the root; each later crumb's `dir` is the
// cumulative path to navigate to when clicked.
export function crumbs(dir) {
  const base = normDir(dir);
  const out = [{ name: "media", dir: "" }];
  if (!base) return out;
  let acc = "";
  for (const seg of base.split("/")) {
    if (!seg) continue;
    acc = acc ? acc + "/" + seg : seg;
    out.push({ name: seg, dir: acc });
  }
  return out;
}

// parentDir returns the directory one level up from `dir` ("" at/above root) —
// the target of the ".." row.
export function parentDir(dir) {
  const base = normDir(dir);
  const slash = base.lastIndexOf("/");
  return slash === -1 ? "" : base.slice(0, slash);
}

// joinDir appends a child segment to a directory, producing the slash path used
// as the next current-directory state when entering a folder.
export function joinDir(dir, seg) {
  const base = normDir(dir);
  return base ? base + "/" + seg : seg;
}
