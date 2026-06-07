# A — identity & config

Source of truth: [docs/README.md](../README.md) §1 (node identity), §2 (ports,
flags/env, data/media dirs), §8.5 (`ENSEMBLE_OUTPUT` backend override). Shared
contracts: [S-skeleton.md](S-skeleton.md) — this piece consumes only
`internal/id` and stdlib.

Scope: parse flags + env fallbacks; resolve `DATA_DIR`/`MEDIA_DIR`; create or
load `node.json` (immutable `id`, renameable `name`); atomic rename-rewrite;
expose the `ENSEMBLE_OUTPUT` sink override. **Pure and unit-testable** — no
sockets, no goroutines, no hardware. Capability *detection* (playback-backend
probe, codec/format lists) is NOT this piece (sink/audio/main own it); config
only carries the persistent identity and the resolved knobs `main` wires in.

Design rule: one small package, two files of code, plain `os`/`encoding/json`.
No abstraction beyond a `Config` value and a `Store` for the JSON file.

---

## 1. Package / file layout

Piece A creates and owns `internal/config/*` (replaces the S stub
`internal/config/config.go`):

```
internal/config/config.go        Config struct, Load() (flags+env+dirs+node.json), Options, defaults
internal/config/node.go          NodeFile (id,name), Store: read/create/atomic-rewrite of node.json
internal/config/config_test.go   flag/env precedence, dir resolution, ENSEMBLE_OUTPUT, --name first-start
internal/config/node_test.go     create-on-missing immutable id, load, rename atomic rewrite, corrupt-file
```

No other files. `cmd/ensemble/main.go` (piece K) calls `config.Load` once at
startup and reads the result; it owns the actual `flag.FlagSet` invocation by
passing `os.Args[1:]` (or its own set) in — see `Options.Args`.

---

## 2. Concrete Go API

### 2.1 `internal/config/config.go`

```go
package config

import (
	"ensemble/internal/id"
)

// Defaults (spec §2). Ports are bases; bind-or-increment happens in netx/main.
const (
	DefaultHTTPPort   = 8080
	DefaultStreamPort = 9090
	DefaultGossipPort = 7946
	DefaultDataDir    = "data"      // relative to CWD if not overridden
	DefaultOutput     = ""          // "" = auto-detect backend (sink piece decides)
)

// Env var names (spec §2, §8.5). Flags override env; env overrides defaults.
const (
	EnvHTTPPort   = "ENSEMBLE_HTTP_PORT"
	EnvStreamPort = "ENSEMBLE_STREAM_PORT"
	EnvGossipPort = "ENSEMBLE_GOSSIP_PORT"
	EnvDataDir    = "ENSEMBLE_DATA_DIR"
	EnvMediaDir   = "ENSEMBLE_MEDIA_DIR"
	EnvOutput     = "ENSEMBLE_OUTPUT" // sink backend override: "", "null", "file", "pw-play", ...
)

// Config is the fully-resolved startup configuration. All fields are final:
// flags+env precedence applied, dirs made absolute, node.json loaded/created.
// Plain value; safe to copy. Holds no open resources.
type Config struct {
	// Identity (from node.json; see Store).
	NodeID   id.ID  // immutable, persisted (§1)
	NodeName string // current name; first 8 hex of id on first start (§1)

	// Resolved, absolute directories (§2).
	DataDir  string // e.g. /abs/data; contains node.json
	MediaDir string // e.g. /abs/data/media; default DataDir/media

	// Base ports (§2). Actual bound ports are decided later by netx (K).
	HTTPPort   int
	StreamPort int
	GossipPort int

	// Sink backend override (§8.5). "" => auto-detect; "null" forces null
	// backend (tests, headless). The sink piece (E) interprets the value;
	// config only carries it.
	Output string

	// store is the node.json handle for runtime renames. Unexported; use Rename.
	store *Store
}

// Options lets the caller (main, tests) inject argv and an env lookup so the
// package is testable without touching the real process environment.
type Options struct {
	Args   []string                 // flag arguments, e.g. os.Args[1:]
	Getenv func(key string) string  // nil => os.Getenv
	// Name is the --name flag pre-set for tests; normally parsed from Args.
}

// Load resolves configuration in this order, then opens/creates node.json:
//
//  1. parse flags from opts.Args into a fresh flag.FlagSet,
//  2. for each knob: flag value if set, else env (opts.Getenv), else default,
//  3. resolve DataDir/MediaDir to absolute paths; MediaDir defaults to
//     DataDir/media,
//  4. MkdirAll(DataDir, 0o755) and MkdirAll(MediaDir, 0o755),
//  5. open node.json via Store: on first start create it with a fresh id.New()
//     and name = --name (if given) else first 8 hex chars of the id; on later
//     starts load it and IGNORE --name (spec §2: "--name only applied on first
//     start").
//
// Returns the resolved Config. Errors are fatal to main: bad flag, unwritable
// data dir, corrupt node.json.
func Load(opts Options) (*Config, error)

// Rename changes the node name and atomically rewrites node.json. It updates
// c.NodeName on success only. The replicated copy (cluster SetName) is the
// caller's responsibility (§1/§4); config owns only the on-disk persistence.
func (c *Config) Rename(name string) error

// NodeFilePath returns DataDir/node.json (for logs / tests).
func (c *Config) NodeFilePath() string
```

Flag names (registered in `Load` on a private `flag.FlagSet`, spec §2):

```
--http-port    int     default DefaultHTTPPort
--stream-port  int     default DefaultStreamPort
--gossip-port  int     default DefaultGossipPort
--data         string  default ""   (=> env ENSEMBLE_DATA_DIR, else DefaultDataDir)
--media        string  default ""   (=> env ENSEMBLE_MEDIA_DIR, else DataDir/media)
--name         string  default ""   (initial node name; first start only)
```

`--output` is intentionally **not** a flag (spec §2 lists no output flag; §8.5
calls it `ENSEMBLE_OUTPUT` env only). Config reads it from env exclusively.

Precedence implementation note: the `flag` package can't tell "default" from
"explicitly set to the default". `Load` registers flags with the **zero
sentinel** (`0` for ports, `""` for strings) as the flag default, parses, then
applies: `if flag != sentinel { use flag } else if env != "" { use env } else {
use real default }`. Port `0` as an explicit value is meaningless here (we want
real port numbers), so `0` safely means "unset". This keeps env fallback
working without a custom `flag.Value`.

### 2.2 `internal/config/node.go`

```go
package config

import (
	"ensemble/internal/id"
)

// nodeFileName is the fixed basename inside DataDir.
const nodeFileName = "node.json"

// NodeFile is the on-disk identity document (§1). Exactly these two fields are
// persisted; everything else in the node record (addrs, ports, caps, following,
// observed) is runtime/replicated state owned by the cluster piece (C), NOT
// stored here.
type NodeFile struct {
	ID   id.ID  `json:"id"`   // immutable, 32-hex (id.ID TextMarshaler)
	Name string `json:"name"` // renameable
}

// Store owns a single node.json file. One Store per node. Methods are safe for
// sequential use from one goroutine; concurrent renames are the caller's
// problem (in practice only the API handler renames, serialized by the cluster
// mutex upstream). Holds the directory path, not an open fd.
type Store struct {
	path string // DataDir/node.json (absolute)
}

// NewStore returns a Store for dataDir/node.json. Does not touch disk.
func NewStore(dataDir string) *Store

// LoadOrCreate reads node.json. If it does not exist, it creates one with a
// fresh id.New() and name = initialName (or first 8 hex of the id when
// initialName == ""). If it exists, it is parsed and returned UNCHANGED — the
// id is immutable (§1) and initialName is ignored on an existing file. A file
// that exists but is empty/corrupt/has a malformed id is an error (we never
// silently regenerate an id, which would orphan the node's cluster identity).
func (s *Store) LoadOrCreate(initialName string) (NodeFile, error)

// Rename writes a new name while preserving the immutable id, via atomic
// replace (write temp in same dir, fsync, os.Rename over node.json). The id
// argument MUST equal the persisted id; a mismatch is ErrIDImmutable (defensive:
// rename never changes identity). Returns the written NodeFile.
func (s *Store) Rename(nodeID id.ID, name string) (NodeFile, error)

// Path returns the absolute node.json path.
func (s *Store) Path() string

var (
	ErrCorruptNodeFile = errors.New("config: node.json is corrupt")
	ErrIDImmutable     = errors.New("config: node id is immutable")
)
```

`defaultName(id.ID)` helper (unexported): `id.String()[:8]` — first 8 hex chars
(§1).

Atomic-rewrite detail (the spec's "atomic rewrite on rename", §1/§2): write to
`node.json.tmp-<rand>` in the **same directory** (so `os.Rename` is on one
filesystem), `f.Sync()` then `f.Close()`, then `os.Rename(tmp, path)`. On any
error, `os.Remove(tmp)` best-effort and return the error; the old file is
untouched. JSON is `MarshalIndent` with a trailing newline for human edits.

---

## 3. Control flow, goroutines, locking

Entirely synchronous; **no goroutines, no channels, no background work**. Piece
A is leaf code.

### Startup (called once by main, K)
1. `cfg, err := config.Load(config.Options{Args: os.Args[1:]})`.
2. `Load` parses flags, applies env fallback, resolves+creates dirs, then
   `Store.LoadOrCreate(name)` to get `NodeID`/`NodeName`, and stashes the
   `*Store` in `cfg.store`.
3. main reads `cfg.NodeID`, `cfg.NodeName`, ports, dirs, `cfg.Output` and wires
   the rest of the system (cluster gets `NodeID`/`NodeName`; netx gets the base
   ports; sink gets `Output`; media listing gets `MediaDir`).

### Steady state
- No state in this package after `Load` returns, except the `*Store` path.
- `cfg.Rename(name)` is called by the API rename handler (`PATCH /api/node`,
  §9.1). It does an atomic file rewrite and updates `cfg.NodeName`. The API then
  also calls `cluster.SetName` to replicate (§4); ordering is the API's choice,
  but persist-then-replicate is recommended so a crash never replicates a name
  that isn't on disk.

### Shutdown
- Nothing to close. `Config` holds no fds, no sockets, no goroutines.

### Locking
- **None inside the package.** `Store.Rename` is not internally synchronized;
  callers serialize renames (in practice a single API handler under the cluster
  mutex). This honors S's "one mutex per stateful component" convention by
  having *zero* shared mutable state to guard — the atomic `os.Rename` is the
  only consistency primitive needed. If concurrent renames ever arise, the
  loser's atomic replace simply wins last; the file never tears.

---

## 4. Edge cases & failure handling

- **First start vs restart (§1, §2):** `LoadOrCreate` distinguishes by
  `os.Stat`. Missing → create with `id.New()` and `--name`-or-default. Present →
  load and **ignore `--name`** (spec §2: "only applied on first start"). The id
  is never regenerated for an existing file.
- **Immutable id (§1):** `Rename` re-reads (or is given) the persisted id and
  refuses to change it (`ErrIDImmutable`); only `name` is written. No API path
  can mutate the id.
- **Corrupt / empty node.json (§1):** parse failure, missing/blank `id`, or an
  id that fails `id.Parse` → `ErrCorruptNodeFile`, fatal. We never silently mint
  a new id over a corrupt file, because that would split the node's cluster
  identity and group memberships. Operator must fix/remove the file.
- **DataDir/MediaDir resolution (§2):** both made absolute via
  `filepath.Abs`. `MediaDir` defaults to `filepath.Join(DataDir, "media")` when
  neither `--media` nor `ENSEMBLE_MEDIA_DIR` is set. Both are `MkdirAll`'d
  (0o755); a path that exists as a non-directory, or an unwritable parent, is a
  fatal error from `Load`.
- **Flag vs env precedence (§2):** flag (if explicitly set) > env (if non-empty)
  > default. Implemented with the zero-sentinel trick (§2.1). An env var set to
  a non-numeric port (`ENSEMBLE_HTTP_PORT=abc`) → error from `strconv.Atoi`,
  fatal. A port out of range (`<1` or `>65535`) → error.
- **`--name` empty AND first start:** name = `id.String()[:8]` (§1 default).
- **`ENSEMBLE_OUTPUT` (§8.5):** passed through verbatim to `cfg.Output`. Config
  does **not** validate the value — the sink piece (E) maps `""`→auto,
  `"null"`/`"file"`/`exec-name` and errors on unknown. Keeps config dumb and
  avoids duplicating the backend list.
- **Atomic rename failure:** temp write or `os.Rename` error leaves the old
  `node.json` intact and `cfg.NodeName` unchanged; the error propagates to the
  API caller which returns 5xx. Best-effort `os.Remove` of the temp file.
- **Relative paths in argv:** resolved against the process CWD at `Load` time
  (documented; main does not `chdir`). Tests pass absolute temp dirs.
- **Path traversal:** out of scope here — `node.json` basename is fixed;
  `MEDIA_DIR` traversal rejection (§6) lives in the media-listing handler (I),
  not config.

---

## 5. Test plan

All tests are hermetic: `t.TempDir()` for dirs, an injected `Getenv` map and an
explicit `Args` slice — no real env, no real flags, no network, no hardware.

`internal/config/node_test.go`
- `TestLoadOrCreateMintsIDOnMissing` — empty dir → file created, id non-zero,
  name == first 8 hex of id when initialName == "".
- `TestLoadOrCreateUsesInitialName` — missing file + initialName "kitchen" →
  name "kitchen", id minted.
- `TestLoadOrCreateLoadsExisting` — write a NodeFile, reload → same id+name,
  initialName ignored.
- `TestLoadOrCreateImmutableIDOnRestart` — create, capture id, call again with a
  different initialName → id unchanged, name unchanged (restart semantics).
- `TestLoadOrCreateCorruptFile` — write "{garbage" → ErrCorruptNodeFile.
- `TestLoadOrCreateMissingID` — write `{"name":"x"}` → ErrCorruptNodeFile.
- `TestLoadOrCreateBadIDHex` — write `{"id":"zz","name":"x"}` →
  ErrCorruptNodeFile.
- `TestRenamePreservesID` — create, Rename to "den", reload → id same, name
  "den".
- `TestRenameAtomicTrailingNewlineValidJSON` — after Rename, file parses and
  ends in "\n"; no `.tmp` files left in the dir.
- `TestRenameRejectsIDMismatch` — Rename with a different id arg →
  ErrIDImmutable, file unchanged.
- `TestRenameOverwritesNotAppends` — two renames → file contains exactly one
  JSON object (no concatenation / tearing).

`internal/config/config_test.go`
- `TestLoadDefaults` — empty Args, empty env → default ports, DataDir abs of
  "data", MediaDir == DataDir/media, Output "".
- `TestLoadFlagsOverrideEnv` — Args set --http-port 9000, env
  ENSEMBLE_HTTP_PORT=1234 → 9000.
- `TestLoadEnvFallback` — no flag, env ENSEMBLE_STREAM_PORT=9100 → 9100.
- `TestLoadDataDirEnv` — ENSEMBLE_DATA_DIR=tmp → DataDir abs(tmp), node.json
  there.
- `TestLoadMediaDirDefault` — only DataDir set → MediaDir == DataDir/media,
  created.
- `TestLoadMediaDirEnvOverride` — ENSEMBLE_MEDIA_DIR=elsewhere → that absolute
  path, created; not under DataDir.
- `TestLoadDataDirMadeAbsolute` — relative --data resolves via filepath.Abs.
- `TestLoadOutputEnv` — ENSEMBLE_OUTPUT=null → cfg.Output == "null".
- `TestLoadNameFirstStartOnly` — Load with --name "a" mints; second Load same
  dir with --name "b" → name still "a".
- `TestLoadBadPortEnv` — ENSEMBLE_HTTP_PORT=abc → error.
- `TestLoadPortOutOfRange` — --gossip-port 70000 → error.
- `TestLoadBadFlag` — Args ["--nope"] → error (FlagSet parse error surfaced).
- `TestLoadUnwritableDataDir` — DataDir points at an existing regular file →
  MkdirAll error (skip if running as root).
- `TestConfigRenamePersistsAndUpdatesField` — Load, cfg.Rename("hall") →
  cfg.NodeName "hall" and reload from disk shows "hall".

---

## 6. Contract summary

| Consumes from S | How |
|---|---|
| `internal/id` | `id.New()` (mint), `id.Parse`/`String` via `id.ID` JSON `TextMarshaler` in `NodeFile`, `id.String()[:8]` default name |

| Produced for downstream | Consumer |
|---|---|
| `Config{NodeID, NodeName, DataDir, MediaDir, *Port, Output}` | K (main) wiring |
| `Config.Rename` | I (API `PATCH /api/node`), paired with `cluster.SetName` |
| `MediaDir` | I (media listing, §6) |
| `Output` | E (sink backend select, §8.5) |
| `NodeID`/`NodeName` | C (cluster seeds own node record, §4) |

This piece introduces **no new cross-piece interface**; it produces a plain
`*Config` value that `main` reads and distributes. Nothing here is on a hot
path, holds a lock, or runs a goroutine.
