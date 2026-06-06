# 03 — Adoption, takeover, forget, security & PKI

> **Scope.** This is the security spine of Ensemble. It specifies the cluster PKI
> (the CA, node keypairs/certs), the **adoption** handshake (PIN challenge-response
> on first contact), **takeover** (re-adopting a node bound to another/old cluster),
> **forget** (revocation), the **mTLS** construction for the control plane, the
> source-IP **allowlist** that is the only guard on the unauthenticated realtime
> planes, the **UI/API auth** model (admin password + sessions + API keys, with the
> node path separated from the human path), and a **threat model** that is honest
> about the limits of a 4-digit PIN.
>
> **Authority.** This document elaborates — and must not contradict — the spine:
> decisions **D9** (adoption PIN), **D10** (mTLS), **D11** (UI/API auth), **D18**
> (CA key custody: no sealing — the CA private key is replicated to full nodes in
> plaintext under a limited LAN threat model), the contracts in
> [README.md §6.5](./README.md) (`ConfigDoc`, `NodeRecord.CertPEM`,
> `NodeRecord.Addrs`, the plaintext-replicated `ClusterSecrets`) and
> [§6.6](./README.md) (API conventions). All endpoint
> request/response shapes are owned by [08](./08-http-api-reference.md); this
> document owns the *protocol and crypto* those endpoints carry. Discovery states
> (`uninitialized` / `foreign` / `member`) are owned by
> [02 §2.4](./02-cluster-discovery-membership.md). The replicated `ConfigDoc` schema,
> merge, persistence, and allowlist *derivation* are owned by
> [07](./07-config-and-replication.md); UI flows by [09](./09-ui-screens.md).

Cross-reference map for this document:

| You want | Go there |
|---|---|
| `ConfigDoc` / `NodeRecord` / `ClusterInfo` / `AuthConfig` schema | [README.md §6.5](./README.md), [07](./07-config-and-replication.md) |
| Wire shapes of `/bootstrap/*`, `/api/v1/adopt`, `/takeover`, `/forget`, `/login`, `/apikeys` | [08](./08-http-api-reference.md) §A, §B, §C |
| Discovery states `uninitialized`/`foreign`/`member`, gossip key sourcing | [02 §2.4, §3.2](./02-cluster-discovery-membership.md) |
| Allowlist derivation from `Addrs ∪ members`, persistence | [07](./07-config-and-replication.md) |
| Setup wizard / Cluster / Settings screens | [09](./09-ui-screens.md) |
| Clock/audio realtime planes that the allowlist guards | [04](./04-clock-and-groups.md), [05](./05-audio-streaming-protocol.md) |

---

## 0. The three trust planes (recap, then refine)

[README.md §4](./README.md) defines three traffic tiers. Their **trust roots**
differ, and that difference is the whole of this document:

| Plane | Transport | Trust root | Auth mechanism | Confidentiality |
|---|---|---|---|---|
| **Control** | HTTP/1.1+2 over TLS | **cluster CA** (D10) | mTLS client+server cert *(node)* **or** session/API key *(human)* | TLS (encrypted) |
| **Clock** | UDP, per group | **allowlist** (source IP) | none — IP gate only (D2/D6) | none (cleartext) |
| **Audio** | UDP unicast (TCP fallback), per group | **allowlist** (source IP) | none — IP gate only (D2/D5) | none (cleartext) |
| **Gossip** | memberlist SWIM/UDP+TCP | **cluster gossip key** (symmetric, AES-GCM) | encrypted-join *is* the gate (D8) | encrypted (key itself plaintext-replicated to full nodes, D18) |
| **Bootstrap** | HTTP over *self-signed* TLS | **the PIN** (D9) for auth; **ECDH** for confidentiality | ECDH + PIN-keyed-HMAC challenge-response (this doc §2.3/§3, [A.9](./A-appendix-algorithms-and-pinned-choices.md)) | self-signed TLS + **ECDH-keyed AEAD** of secrets (confidentiality rests on ECDH, not the PIN — §2.3) |

Two observations drive the design:

1. **The realtime planes have no cryptographic auth.** A 480-sample PCM/Opus frame every
   10 ms cannot afford per-packet MAC/decrypt on a Pi Zero (and dumb nodes, D15, can
   afford it even less). The *only* protection is the source-IP allowlist (§6). This
   is a deliberate, locked trade-off (D2). We compensate by deriving the allowlist
   strictly from cryptographically-adopted members and by bounding blast radius
   (§6.4, §8).

2. **The control plane is the trust origin.** mTLS membership *is* the cluster: a
   node holds a CA-signed leaf or it is not a peer. Adoption is therefore the act of
   getting a leaf signed; forget is the act of making a leaf untrusted. Everything
   else hangs off the CA.

---

## 1. PKI — the cluster CA and node identities

### 1.1 Where the CA is born: first-init

A cluster is created by exactly one act: `POST /api/v1/setup`
([08 §B.1](./08-http-api-reference.md)) on an uninitialized node. At that moment the
`internal/pki` package:

1. Generates the **cluster CA** keypair (Ed25519; see §1.5 for the algorithm choice).
2. Self-signs a CA certificate: `CN=ensemble-ca/<clusterName>`, `IsCA=true`,
   `BasicConstraintsValid=true`, `MaxPathLen=0` (the CA signs leaves only, never
   intermediates), `KeyUsage = CertSign | CRLSign`, validity **10 years**.
3. Generates this founding node's **leaf** keypair + self-issues its leaf from the
   brand-new CA (the node adopts *itself*; see §2.6).
4. Writes the CA **public** cert into `ConfigDoc.Cluster` (the spine field
   `ClusterInfo` — "name, CA cert (public), created", [README.md §6.5](./README.md)),
   sets `ConfigDoc.Version = 1`, and persists.
5. Mints the **`ClusterSecrets`** material — the CA **private** key (`caKeyPEM`) and a
   random **gossip key** (§1.6) — and replicates it **in plaintext** to full nodes
   (D18; §1.2), and writes the **admin password** hash into `ConfigDoc.Auth` (§7.1).

```go
// internal/pki
type CA struct {
    Cert    *x509.Certificate // public; mirrored into ConfigDoc.Cluster
    certPEM []byte
    key     crypto.Signer      // PRIVATE — see §1.2 for where this lives
}

// CreateCA mints a fresh cluster CA on first-init. Returns the CA (with its
// private key in memory) and the PEM to publish into ConfigDoc.Cluster.
func CreateCA(clusterName string, now time.Time) (*CA, error)

// Sign issues a leaf certificate for a CSR. Used by adoption (§2), takeover (§4),
// and renewal (§1.4). nodeID becomes the CN; addrs become IP SANs; id becomes a
// URI SAN. validity is short (§1.4).
func (ca *CA) Sign(csr *x509.CertificateRequest, nodeID string, addrs []net.IP, validity time.Duration, now time.Time) (certPEM []byte, err error)
```

The CA **public** cert in `ConfigDoc.Cluster` is the *distribution* mechanism: every
node that merges the `ConfigDoc` (via gossip, [07](./07-config-and-replication.md))
thereby learns the CA it must verify peers against. No separate CA-bundle channel is
needed for *steady state*; the bundle is only handed out-of-band during the bootstrap
handshake (§2), to a node that does not yet have the `ConfigDoc`.

### 1.2 Where the CA *private key* lives — the central trade-off

The CA must sign **future** adoptions. The spine requires that "every node hosts a
full UI that can operate the *whole* cluster (adopt/takeover/forget)"
([README.md §1](./README.md)) and that the **Controller** is "whatever node is
currently driving an operation (any adopted node can)" ([README.md §2](./README.md)).
That is in direct tension with the security instinct to keep a CA key in exactly one
place. Three models:

| Model | CA key location | Pro | Con |
|---|---|---|---|
| **A. Controller-only** | One designated node holds the key | Smallest key-exposure surface | Contradicts "any node can adopt"; single point of failure for adoption; needs leader election just to sign |
| **B. Any-full-node-can-sign (replicated, plaintext)** | Every full node holds the CA key, **stored in plaintext** at rest (D18) | Matches the spine's "any node can drive any op"; no adoption SPOF; survives loss of any node; trivial (no at-rest crypto) | Largest exposure surface; **read access to any full node's disk yields the CA key** |
| **C. Replicated key, sign-on-quorum** | Every full node holds a share; signing needs k-of-n | Strong against single-node compromise | Heavy (threshold crypto), bad fit for dumb nodes (D15), brittle on a flaky LAN |

**Decision (this doc, locking D18): Model B — any full node can sign, CA key
replicated to full nodes in plaintext, with NO sealing/encryption-at-rest.**
Justification:

- It is the only model consistent with the locked spine ("any adopted node can"
  drive adopt/takeover/forget). Choosing A or C would silently contradict
  [README.md §1/§2](./README.md), which this doc may not do.
- Adoption is a rare, human-gated (PIN), single-segment LAN operation. The cost of a
  cluster-wide SPOF (Model A) — you cannot adopt while the one CA node is down — is
  judged worse than the exposure of Model B on a *trusted home LAN*, which is the
  only deployment in scope ([README.md §1 non-goals](./README.md): no
  internet/cloud/multi-site).
- **D18 drops at-rest sealing entirely.** Under the limited LAN threat model the
  marginal protection that encryption-at-rest bought (a forgotten node, or a disk
  read, recovering only ciphertext) is not worth its machinery: a live full node
  must hold the key in cleartext in memory to sign anyway (the old "sealing" only
  ever protected disk-at-rest, never a live-rooted node). We therefore store the CA
  key as plaintext PEM and replicate it as such to full nodes.
- The honest residual risk (read access to *any* full node ⇒ CA compromise) is
  **accepted** and recorded in the threat model (§8, "lost/compromised CA key") with
  its recovery procedure (§1.7 CA rotation).

**How the CA key is replicated.** The CA *private* key is **not** a public field and
therefore is **not** carried in the normal public `ConfigDoc` projection (that doc
carries only the **public** CA cert and **public** node certs — see
[README.md §6.5](./README.md), `NodeRecord.CertPEM` is explicitly "(pub)"). Instead it
travels as plaintext replicated **`ClusterSecrets`** material (field name coordinated
with [07](./07-config-and-replication.md); see [README.md §6.5](./README.md)):

- `ClusterSecrets` is the replicated **secret** projection — `{caKeyPEM, gossipKey, …}`
  — replicated to **full nodes** in plaintext (D18). It is a sibling replicated value
  to the public `ConfigDoc`; it is persisted **0600**, never logged, and never
  returned by any API. Persistence/merge of this value is owned by
  [07](./07-config-and-replication.md); this document owns only what it contains and
  how adoption delivers it (§2.4).
- `caKeyPEM` is the CA private key as plaintext PEM. There is **no** seal key, no
  AEAD blob, no `caKeySealed`: any full node that holds `ClusterSecrets` loads
  `caKeyPEM` directly and can sign.

```go
// internal/pki — the CA key is loaded/stored as plaintext PEM (D18: no sealing).
func MarshalCAKey(caKey crypto.Signer) (pemBytes []byte, err error) // -> ClusterSecrets.caKeyPEM
func ParseCAKey(pemBytes []byte) (crypto.Signer, error)             // <- ClusterSecrets.caKeyPEM
```

> **Limit, stated plainly (D18).** Because the CA key is plaintext on every full
> node's disk, **anyone with read access to a full node's disk gets the CA key and
> can mint certs** (i.e. forge cluster membership). This is **accepted** for a
> trusted LAN. The mitigations are non-cryptographic: keep full nodes
> physically/administratively trusted, and rotate the CA (§1.7) after any suspected
> node compromise. **Revisit this decision if the threat model ever widens** beyond
> a single trusted LAN (per-device sealing, an HSM, or Model C would then be on the
> table).

### 1.3 Node keypair, CSR, and certificate fields

Each node generates its **own** keypair locally and **never** transmits the private
key. Adoption transmits only a CSR (public) and receives back a signed leaf (public).

**CSR contents** (`internal/pki.NewCSR`):

| CSR field | Value |
|---|---|
| Subject `CN` | the node id (`n-7a3f`) |
| `PublicKey` | the node's freshly generated public key (Ed25519) |
| (SANs are **not** trusted from the CSR) | The CA *ignores* SANs in the CSR and sets them itself from authenticated inputs — see below |
| Signature | self-signature proving possession of the private key |

**Issued leaf certificate fields** (set by `CA.Sign`, *not* copied from the CSR):

| Cert field | Value | Why |
|---|---|---|
| `Subject.CN` | node id | stable identity; mTLS peer = `CN` |
| SAN URI | `ensemble://node/<nodeId>` | the canonical, address-independent identity used for peer-identity checks (§5.2) |
| SAN IP | every IP in the node's authenticated `Addrs` | lets standard TLS hostname verification pass when peers dial by IP; keeps cert ↔ allowlist consistent |
| SAN DNS | `<nodeId>.ensemble.local` | optional mDNS name match |
| `KeyUsage` | `DigitalSignature` | leaf, not a CA |
| `ExtKeyUsage` | `ServerAuth, ClientAuth` | the same leaf is used as **both** server and client cert (mTLS both directions, §5) |
| `NotBefore/NotAfter` | now − 5 min … now + **30 days** | short-lived (§1.4); 5-min backdate absorbs clock skew before clock sync converges |
| `SerialNumber` | random 128-bit | unique per cert (revocation keys on the SHA-256 fingerprint, not the serial — §5.2) |

The CA deliberately sets SANs from **authenticated** data (the node id assigned by
the controller, the addrs the controller observed/was told) rather than from the CSR,
so a node cannot request a cert that impersonates another's id or address.

```go
// internal/pki
func NewIdentity() (crypto.Signer, error)                 // node keypair (Ed25519)
func NewCSR(key crypto.Signer, nodeID string) (csrPEM []byte, err error)
```

### 1.4 Short-lived certs + renewal

Leaf validity is **30 days** (D9 "short-lived"); renewal begins at **1/3 of
lifetime** (day 10 of 30) — canonical in
[A.12](./A-appendix-algorithms-and-pinned-choices.md). Renewal is *not* a re-adoption —
it needs no PIN — because the node already holds a valid leaf and can authenticate over
mTLS:

```
node (mTLS, current leaf)                          any full node (holds CA key)
   │  POST /api/v1/nodes/{id}/renew  { csrPem }       │
   │  (auth class: mTLS-node; CN must equal {id})     │
   │ ───────────────────────────────────────────────►│  CA.Sign(csr, id, addrs, 30d)
   │ ◄─────────────────────────────────────────────── │  { signedCertPem }
   │ persist new leaf; update NodeRecord.CertPEM      │  gossip ConfigDoc (LWW)
```

The renewing node also writes its new public `CertPEM` into its own `NodeRecord`
(via the normal config-write path, [07](./07-config-and-replication.md)) so peers
learn the rotated cert through gossip. Short lifetime is the backbone of the
revocation strategy (§4-forget, §5.3): a forgotten node's cert simply *expires*, and
until then a revoked-set blocks it.

> **`/renew` is mTLS-node-authenticated** and the requested `CN` must equal the
> caller's own mTLS-cert `CN`; a node may only renew *itself*. It is not in the
> human/API surface.

### 1.5 Algorithm choices

| What | Choice | Why |
|---|---|---|
| CA + leaf keys | **Ed25519** | small keys/sigs (good for the dumb-node future, D15), fast verify, no parameter foot-guns |
| Cert signature | Ed25519 | same |
| TLS | **TLS 1.3 only** (`tls.Config.MinVersion = VersionTLS13`) | modern AEAD suites only, no downgrade, simpler config |
| Gossip transport | **AES-256-GCM** (memberlist `SecretKey`) | hardware-accelerated, AEAD (no at-rest sealing — D18; the gossip key itself is plaintext-replicated) |
| KDF | **HKDF-SHA256** | matches the reused mpvsync `internal/auth` HKDF pattern (`key.go`); derives both the ECDH confidentiality key `k` and the PIN-keyed auth key `kp` (§2.3) |
| Adoption confidentiality | **X25519 ECDH** (`crypto/ecdh`) + **ChaCha20-Poly1305** | CA-bundle secrecy rests on the ECDH shared secret, not the PIN (§2.3, A.9) |
| Admin password | **argon2id** (§7.1) | memory-hard, the spine names it (D11/§7) |
| PIN handshake | **ECDH (confidentiality) + PIN-keyed HMAC over the transcript (auth)** (§2.3, A.9) | implementable without a PAKE dependency; see §3.5 for the SPAKE2/CPace alternative and the honest limit |

### 1.6 The cluster gossip key (plaintext-replicated)

The **gossip key** (`gossipKey`, 32 random bytes) is minted at first-init and feeds
memberlist's `SecretKey` ([02 §3.2](./02-cluster-discovery-membership.md)). Under D18
there is **no cluster-root key hierarchy and no sealing**: the gossip key is simply
generated and **replicated to full nodes in plaintext** as part of `ClusterSecrets`
(§1.2), exactly like `caKeyPEM`. There is no derived seal key — the CA key it would
have protected is itself plaintext now (§1.2).

```
ClusterSecrets (minted at setup, replicated to full nodes in PLAINTEXT — D18)
   ├─ caKeyPEM   (CA private key PEM)  → CA.Sign (§1.1/§2/§4)
   └─ gossipKey  (32B random)          → memberlist SecretKey (02 §3.2)
```

This satisfies [02 §3.2](./02-cluster-discovery-membership.md)'s requirement that the
gossip key be "a cluster-wide secret distributed during adoption alongside the CA":
that secret is the `ClusterSecrets` bundle (`gossipKey` + `caKeyPEM`), handed to an
adoptee inside the PIN-protected bootstrap response (§2.4) and replicated thereafter.

```go
// internal/pki (or internal/auth) — cluster gossip key (random; D18: no derivation/sealing).
func NewGossipKey() []byte // 32 random bytes -> ClusterSecrets.gossipKey
```

### 1.7 CA rotation (recovery from CA compromise)

Because Model B spreads the CA key, "rotate the CA" is the recovery path after any
suspected full-node compromise:

1. An operator triggers rotation; `internal/pki` mints a **new** CA + new
   `ClusterSecrets` (new `caKeyPEM` + new `gossipKey`).
2. The new CA cross-signs nothing; instead every node **re-issues** its leaf from the
   new CA via the `/renew` path, now authenticated by the *old* still-valid mTLS
   plus the operator session.
3. Once all live members carry new-CA leaves, the cluster **rekeys** gossip
   (`Membership.Rekey`, reused from mpvsync `membership.go:180`,
   [02 §3.2](./02-cluster-discovery-membership.md)) and drops the old `ClusterSecrets`
   (old `caKeyPEM` + old `gossipKey`).
4. Any node not present for rotation is effectively forgotten (its old leaf no longer
   chains to the live CA, and it cannot decrypt the new gossip) — i.e. rotation is
   the big hammer that also evicts a compromised node.

This is an operator-initiated, full-cluster operation; its UI lives in
**Settings → Cluster info** ([09 §8](./09-ui-screens.md)).

---

## 2. Adoption — bringing an uninitialized node in (D9, the PIN)

### 2.1 The problem of first contact

Before adoption a node has **no** CA-signed identity, so the control plane's mTLS
cannot authenticate it (chicken-and-egg). The node therefore exposes a **bootstrap**
surface (`/bootstrap/*`, [08 §A](./08-http-api-reference.md)) over a **self-signed**
TLS cert, *outside* mTLS. This surface is the one and only thing reachable on an
uninitialized node (§7.4). Its security rests entirely on the **PIN** (D9).

The PIN is the placeholder `"0000"` but **must be treated as a real secret in the
protocol** (D9). Two hard constraints follow:

1. **The PIN must never travel in clear** — not even inside the self-signed TLS,
   because the adopter cannot yet authenticate that TLS cert (no CA), so a
   man-in-the-middle could terminate TLS, learn a cleartext PIN, and adopt the node
   itself.
2. **A 4-digit PIN has only ~13.3 bits of entropy.** No protocol can make 10⁴
   guesses *offline* hard. The realistic, honest goal is: (a) never reveal the PIN on
   the wire, (b) bind the handshake to the exact keys/CSR exchanged so a MITM cannot
   splice, and (c) make **online** guessing slow and detectable. §3.5 is explicit
   about what this cannot achieve.

### 2.2 Pin the self-signed cert first (fingerprint)

Before any PIN material is exchanged, the controller learns the node's self-signed
cert **fingerprint** via `GET /bootstrap/info` ([08 §A.1](./08-http-api-reference.md),
`fingerprint: "sha256:…"`). The operator (or the discovery row, [02 §2.4](./02-cluster-discovery-membership.md))
carries that fingerprint into `POST /api/v1/adopt`
([08 §C.3](./08-http-api-reference.md), field `fingerprint`). The controller then
**pins** that exact cert for the bootstrap TLS connection. This converts the
self-signed channel into a *fingerprint-pinned* channel for the duration of
adoption: a MITM that cannot present the pinned cert is detected before the PIN
proof is computed. The fingerprint is *advisory* against a passive observer but
becomes load-bearing combined with the PIN-keyed transcript (§2.3): the PIN proof is
bound to the pinned cert's key, so a substituted cert breaks the proof.

### 2.3 The PIN challenge-response (concrete scheme — Appendix A.9, normative)

The adoption handshake is specified, **normatively**, by
[Appendix A.9](./A-appendix-algorithms-and-pinned-choices.md). It is an **ephemeral
X25519 ECDH exchange for confidentiality** + a **PIN-keyed HMAC-SHA256 over the
transcript for authentication** + **ChaCha20-Poly1305** AEAD for the secret payload.
The CA-bundle confidentiality rests on the **ECDH shared secret**, *not* on the 4-digit
PIN: a passive eavesdropper who never learns the PIN still cannot read the CA bundle,
because it cannot derive the ECDH secret. The PIN's job is purely **authentication** —
to stop an active MITM from getting a CSR signed or impersonating the controller.

This supersedes the earlier HKDF(PIN)-keyed-AEAD design: confidentiality no longer
hangs off the low-entropy PIN. Replay is defeated by the per-side nonces feeding the
transcript; splice/MITM is defeated because the PIN-keyed HMAC covers the *exact*
ECDH publics, nonces, and CSR in flight.

**Primitives** (all Go stdlib / `x/crypto`, pinned [A.11](./A-appendix-algorithms-and-pinned-choices.md)):
X25519 (`crypto/ecdh`) · HKDF-SHA256 (`x/crypto/hkdf`) · HMAC-SHA256 (`crypto/hmac`) ·
ChaCha20-Poly1305 (`x/crypto/chacha20poly1305`).

**The 6-step exchange (A.9), restated inline.** The uninitialized node holds PIN `p`
(default `"0000"`); the controller is given `p` out-of-band by the operator.

```
1. node:        generate ECDH ephemeral (nA, NA); show/hold PIN p; expose GET /bootstrap/info
2. controller:  generate ECDH ephemeral (nB, NB); POST /bootstrap/adopt { NB, nonceB }
3. node reply:  { NA, nonceA }
4. both:  Z  = X25519(own_priv, peer_pub)                              // ECDH shared secret
          k  = HKDF(Z, salt = nonceA‖nonceB, info = "ensemble-adopt-v1")  // confidentiality key
          kp = HKDF(p, salt = nonceA‖nonceB, info = "ensemble-pin-v1")    // PIN-derived auth key
          transcript = NA‖NB‖nonceA‖nonceB
5. controller -> node:  { csr_request, tag = HMAC(kp, transcript ‖ "req") }
   node verifies tag (rejects if PIN wrong / MITM) -> returns AEAD_k( CSR )    // encrypted under Z
6. controller signs CSR with cluster CA -> node:
                          AEAD_k( signedCert ‖ caBundle ‖ clusterSecrets )      // confidential under Z
                          + tag2 = HMAC(kp, transcript ‖ "done")
   node verifies tag2, decrypts, installs cert+CA -> joins mTLS mesh.
```

The `ClusterSecrets` (`caKeyPEM` + `gossipKey`, §1.6) ride in the step-6 AEAD payload,
encrypted under the **ECDH** key `k` — confidential on the bootstrap wire against a
passive eavesdropper regardless of PIN strength. (This per-handshake AEAD is
bootstrap-transport protection only — at rest and in steady-state replication these
secrets are **plaintext** on full nodes, D18 §1.2.) HMAC tags are verified with
`subtle.ConstantTimeCompare` (the mpvsync `internal/auth` pattern, `controller.go:116`).

### 2.4 Message flow

Wire shapes are owned by [08 §A.2](./08-http-api-reference.md) / [§C.3](./08-http-api-reference.md);
this is the *protocol*. The A.9 6-step exchange (§2.3) is layered on the bootstrap
endpoints (§9): the controller's first `adopt` call swaps ECDH publics + nonces and
returns the AEAD-wrapped CSR; the second carries the HMAC-authenticated, ECDH-encrypted
signed cert + CA bundle + `ClusterSecrets`.

```
 Operator                Controller (holds CA key, Model B)            Uninitialized node N
    │  enters PIN p +          │                                              │
    │  picks node from         │   (1) GET /bootstrap/info ──────────────────►│  gen ECDH (nA,NA)
    │  Discovery (02 §2.4)     │       ◄──── {nodeId, fingerprint fpN, state} │  hold PIN p
    │ ───── POST /api/v1/adopt │                                              │
    │  {nodeId,addr,fpN,pin}   │   pin TLS to fpN for all calls below         │
    │                          │   gen ECDH (nB,NB)                           │
    │                          │                                              │
    │                          │   (2) POST /bootstrap/adopt {NB,nonceB} ─────►│  pin-TLS(fpN)
    │                          │       ◄──── { NA, nonceA }                   │  Z=X25519(nA,NB)
    │                          │   Z=X25519(nB,NA)                            │  k,kp=HKDF(…)
    │                          │   k=HKDF(Z,…); kp=HKDF(p,…)                   │
    │                          │   (3) {csr_request, tag=HMAC(kp,T‖"req")} ───►│  verify tag (PIN/MITM
    │                          │       ◄──── AEAD_k( csrPem )                  │  ── bad? abort §3.4)
    │                          │   decrypt CSR                                │  return AEAD_k(CSR)
    │                          │   ── bad tag? abort, count attempt (§3.4) ─   │
    │                          │   CA.Sign(csr,nodeId,addrs,30d) -> leafPEM    │
    │                          │   payload = leaf‖caBundle‖ClusterSecrets      │
    │                          │                                              │
    │                          │   (4) AEAD_k(payload) + tag2=HMAC(kp,T‖"done")►│  verify tag2
    │                          │       { assignedNodeId, clusterName,         │  decrypt AEAD_k ->
    │                          │         seedPeers, ... }                     │  leaf+CA+ClusterSecrets;
    │                          │       ◄──── { nodeId, state:"member" }       │  persist 0600
    │ ◄─── 200 {version,node}  │   record NodeRecord(CertPEM=leaf, Addrs)     │  (plaintext, D18);
    │      (ConfigDoc bumped)  │   If-Match; gossip to all full nodes (07)    │  close /bootstrap;
    │                          │                                              │  serve /api/v1 mTLS;
    │                          │                                              │  Join(seedPeers)
```

Notes:

- The controller's `POST /api/v1/adopt` ([08 §C.3](./08-http-api-reference.md)) is the
  *human/API* entry; the controller then drives the bootstrap calls to N over the
  pinned channel. The CA signing happens on the **controller** node (it holds the CA
  key, Model B), exactly as [08 §C.3 Proxy](./08-http-api-reference.md) states
  ("proxied to the target … then local config write → gossip").
- Only **public** artifacts (ECDH publics, CSR, certs, ids) and **AEAD-encrypted under
  the ECDH key `k`** secrets (the `ClusterSecrets`: `caKeyPEM` + `gossipKey`) cross the
  bootstrap wire. The adoptee's *own* node private key (and ECDH private) never leave N.
  The PIN itself never crosses the wire — only HMAC tags derived from it. Confidentiality
  rests on ECDH, not the PIN. (The AEAD here protects only the bootstrap transport; once
  installed, these secrets sit in plaintext on the full node, D18 §1.2.)
- Adoption **refuses an epoch mismatch** (m7): if N's `protocolEpoch`
  ([08](./08-http-api-reference.md), via `/bootstrap/info`) differs from the cluster's,
  the controller aborts before signing — mixed-version clusters are unsupported; upgrade
  all nodes together (§2.7). `softwareVersion` is UI-visibility only and does *not* gate
  adoption.
- After the exchange completes, N writes `init=1` + its `cf` (CA fingerprint) into its
  mDNS TXT ([02 §2.4](./02-cluster-discovery-membership.md)), so discovery flips it from
  `uninitialized` to `member`/`discovered`.

### 2.5 Why this resists replay and MITM (and where it doesn't)

| Attack | Defeated by | Residual |
|---|---|---|
| **Passive eavesdrop** | **ECDH confidentiality**: `ClusterSecrets`/CA bundle are AEAD-wrapped under `k = HKDF(Z, …)`, `Z` the X25519 shared secret an eavesdropper cannot derive; PIN never sent | none of note — confidentiality does **not** depend on PIN entropy (at rest these secrets are plaintext per D18 — a *disk* read, not an eavesdrop, is the relevant threat there: §8 T6) |
| **Replay** of a captured handshake | fresh ECDH ephemerals + `nonceA`,`nonceB` per handshake feed `k`, `kp`, and the transcript; old tags don't verify | none |
| **MITM substitutes its own cert** | controller pins `fpN` (§2.2); a different cert breaks the pinned channel before the PIN-keyed tags are exchanged | MITM must *also* know the PIN to forge `tag`/`tag2` |
| **MITM substitutes its own ECDH/CSR** | the PIN-keyed HMAC tags cover the transcript (`NA‖NB‖nonceA‖nonceB`); splicing changes the tag | none without the PIN |
| **Offline PIN brute force** | ECDH already protects confidentiality, so a capture does **not** expose the CA bundle | The PIN-keyed HMAC tags are still offline-testable: a capturer of one handshake can offline-test all 10⁴ PINs against `tag`/`tag2` to confirm the PIN (it cannot decrypt the bundle without `Z`). ~13.3 bits. See §3.5. |
| **Online PIN guessing** | per-node attempt throttle + lockout (§3.4) | bounded, detectable |

### 2.6 Self-adoption (the founding node)

`POST /api/v1/setup` ([08 §B.1](./08-http-api-reference.md)) is the degenerate case:
the node creates the CA (§1.1) and signs its *own* CSR locally — no PIN, no
bootstrap round-trip, because there is no second party and no pre-existing trust to
bridge. It is reachable only while uninitialized (§7.4) and returns `409 conflict`
afterwards.

### 2.7 Protocol epoch — adoption refuses a version mismatch (m7)

All nodes in a cluster run the **same protocol epoch**; mixed-version clusters are
**unsupported** — upgrade together. Adoption (and takeover, §4) therefore **refuses a
node whose `protocolEpoch` differs** from the cluster's: the controller reads the
adoptee's epoch from `GET /bootstrap/info` (§2.2) and **aborts before signing** the CSR
if it does not match, rather than admitting an incompatible peer. This is distinct from
`softwareVersion`, which is **UI-visibility only** and never gates adoption: two nodes
may run different build strings as long as their protocol epoch is identical.

---

## 3. The PIN, rigorously: limits and hardening

### 3.1 Entropy budget

A 4-digit decimal PIN is `log2(10⁴) ≈ 13.29` bits. This is the dominating security
parameter of first contact and **no protocol choice can change it**. The placeholder
`"0000"` is *zero* effective entropy until per-device PINs land (D9 "per-device
provisioning later"). The protocol is built so that swapping in a higher-entropy
secret (per-device PIN, QR-provisioned key) *only* improves §2.5's "offline brute
force" row without any protocol change.

### 3.2 Goals the scheme actually meets

- **Confidentiality of the PIN on the wire:** met (never transmitted; only HMAC tags).
- **Confidentiality of the CA bundle / `ClusterSecrets`:** met by **ECDH**, independent
  of PIN entropy (§2.3) — a passive capturer cannot decrypt them.
- **Handshake integrity / anti-splice:** met (PIN-keyed transcript binding).
- **Anti-replay:** met (fresh ECDH ephemerals + dual nonces).
- **Online-guess resistance:** met by policy (§3.4), not by entropy.

### 3.3 Goals it cannot meet (be honest)

- **Offline-guess resistance against a passive capturer:** **not met** (for the PIN
  *as an authenticator*). An attacker who records one complete handshake holds the ECDH
  publics, nonces, transcript, and the PIN-keyed tags. They can compute
  `kp'=HKDF(guess,…)` and test `HMAC(kp',transcript‖"req")==tag` for all 10⁴ guesses in
  microseconds, confirming the PIN. **Note the CA bundle stays confidential regardless**:
  confidentiality rests on the ECDH secret `Z` (which the capturer cannot derive), not on
  the PIN — so a capture lets the attacker *verify the PIN offline* but **not** decrypt
  the secrets. The *only* real mitigations against the PIN-as-authenticator leak are
  (a) raise PIN entropy (per-device, D9), or (b) a PAKE (§3.5) which removes the
  offline-test oracle — at a dependency cost.
- **No reliance on the human:** the operator must read the *right* PIN off the *right*
  device and adopt the *right* discovery row; a confused-deputy operator can adopt an
  attacker's planted node. Fingerprint display (§2.2) is the only UI mitigation.

### 3.4 Online brute-force hardening (mandatory)

The uninitialized node enforces, per the bootstrap surface (canonical values in
[A.12](./A-appendix-algorithms-and-pinned-choices.md) — referenced here, not restated):

| Control | Value |
|---|---|
| Failed-proof counter | per *source* + global, in memory |
| Soft backoff | after **3 consecutive fails** (`429`, envelope `code:"rate_limited"`, [08 §0.4](./08-http-api-reference.md)) |
| Hard lockout | after **10 fails within 5 min**, bootstrap refuses `adopt` for **15 min** (`/bootstrap/info` still answers) |
| Nonce TTL | a handshake nonce is single-use and expires in **30 s**; the second exchange must quote the matching nonce |
| Audit | every failed proof logged with source IP |

These make the only *feasible* attack (online guessing, since the node is the sole
holder of the verifier) slow and noisy. With `"0000"` and default 0-entropy this is
weak by construction — see §8.

### 3.5 Alternative: SPAKE2 (and why we defer it)

A **PAKE** such as **SPAKE2** (or **CPace**) would fold the PIN itself into the key
agreement so the PIN is *never* an offline-testable verifier: a passive capturer learns
nothing testable about the PIN, and each online attempt costs exactly one guess. The
baseline ECDH scheme (A.9, §2.3) already removes the *confidentiality* dependence on the
PIN — a capture cannot decrypt the CA bundle — but the **PIN-keyed HMAC tags remain
offline-testable** (§3.3); a PAKE closes that last hole and is the *correct* long-term
design.

We **specify the ECDH + PIN-keyed-HMAC scheme as the baseline** because:

- it uses only **already-pinned crypto** (X25519 via stdlib `crypto/ecdh`, plus
  HKDF/HMAC/ChaCha20-Poly1305 from `golang.org/x/crypto` already vendored,
  [A.11](./A-appendix-algorithms-and-pinned-choices.md)) — **no PAKE dependency**;
- it is trivially implementable and auditable, and ECDH already buys passive-eavesdrop
  confidentiality;
- on a trusted home LAN (the only deployment in scope) the dominant risk is a *rogue
  adopter/operator confusion*, not a passive PIN capturer with patience.

The seam is drawn so a PAKE is a drop-in: replace "derive the auth path via
`kp = HKDF(PIN, nonces)` + transcript HMAC" (§2.3, A.9 steps 4–5) with
"derive a shared key via SPAKE2/CPace(PIN)"; the ECDH confidentiality, message flow
(§2.4), and endpoints (§9) are otherwise unchanged. This is recorded as an open item for
[10-roadmap](./10-roadmap-and-dumb-nodes.md) (alongside per-device PINs).

---

## 4. Takeover — re-adopting a node bound to another/old cluster

A node is **`foreign`** when its mDNS TXT advertises a `cf` (cluster fingerprint)
that differs from ours ([02 §2.4](./02-cluster-discovery-membership.md)). Such a node
already holds a leaf from *another* CA and a *different* `ClusterSecrets` (other
cluster's `caKeyPEM`/`gossipKey`). `adopt`
([08 §C.3](./08-http-api-reference.md)) returns `403 forbidden` for it; the operator
must explicitly **takeover** (`POST /api/v1/takeover`,
[08 §C.4](./08-http-api-reference.md), with `force:true`).

Takeover is **adoption with a supersede step**, still PIN-gated (D9):

```
 Controller (our CA)                                   foreign node N (other cluster's CA)
   POST /api/v1/takeover {nodeId,addr,fpN,pin,force:true}
       (1) GET /bootstrap/info ──► state:"foreign"           bootstrap IS still served
            (also: protocolEpoch must match, else abort — §2.7, m7)
       (2) ECDH key+CSR exchange : same A.9 6-step flow as §2.4 (PIN gates it identically)
       (3) sign N's CSR with OUR CA -> new leaf
       (4) complete: push our leaf + our caBundle + OUR      N: ATOMIC supersede —
            ClusterSecrets (caKeyPEM+gossipKey, AEAD under     • drop old leaf, old CA,
            ECDH key k) + seedPeers                              old ClusterSecrets
       ◄── 200                                                  • install new identity
   record fresh NodeRecord (new CertPEM, addrs); If-Match;     • re-key gossip to ours
   gossip to all full nodes (07)                               • leave old cluster's gossip
                                                               • Join(our seedPeers)
```

Key points:

- The **PIN is still required** — takeover does not bypass first-contact auth; it
  only overrides the *membership* check that `adopt` enforces. `force:true`
  ([08 §C.4](./08-http-api-reference.md)) is the operator's explicit consent to
  supersede.
- The node performs an **atomic identity swap**: it must not end up half in two
  clusters. On any failure before `complete` is fully persisted, it stays in its old
  cluster.
- The old cluster is **not** notified (it may be unreachable / decommissioned). From
  the old cluster's perspective the node simply stops gossiping (its gossip key
  changed) and its old leaf will expire (≤30 days, §1.4); the old cluster should
  `forget` it (§5) to clean up immediately.
- Because the node gets *our* `ClusterSecrets` (our `caKeyPEM` + `gossipKey`, §1.6),
  it joins *our* gossip and — holding the plaintext CA key (D18, §1.2) — it too can
  sign future adoptions (Model B). If a foreign node should *not* gain that power,
  that is a per-device capability question deferred to
  [10](./10-roadmap-and-dumb-nodes.md).

---

## 5. Forget — revoking a node (D9 "forget")

`POST /api/v1/nodes/{id}/forget` ([08 §C.5](./08-http-api-reference.md)) removes a
node. Its job is to make the forgotten node **unable to keep talking** on any plane.

### 5.1 What forget changes (all in one `ConfigDoc` write)

| Plane | Action | Effect |
|---|---|---|
| Control (mTLS) | add the node's cert **SHA-256 fingerprint** to `ConfigDoc`'s **`RevokedSet`**; drop its `NodeRecord` | peers reject its cert at the verify callback (§5.2) immediately on merge |
| Realtime (clock/audio) | remove its `Addrs` from the allowlist source (it's no longer in `Nodes[]`) | clock/audio sockets drop its packets (§6) |
| Gossip | trigger a **gossip rekey** so the new key excludes the forgotten node (§5.3) | it can no longer decrypt/participate in gossip |
| Groups | pull the node id from every `GroupRecord.memberNodeIds` | reported in `affectedGroups` ([08 §C.5](./08-http-api-reference.md)) |

The single write is `If-Match`-guarded ([08 §0.5](./08-http-api-reference.md)),
bumps `Version`, and gossips to all full nodes ([07](./07-config-and-replication.md)).
A best-effort mTLS notice is proxied to the target (it may already be gone — non-fatal,
per [08 §C.5 Proxy](./08-http-api-reference.md)).

### 5.2 Revocation strategy: short-lived certs + a replicated revoked-set (no CRL/OCSP)

We **reject CRL and OCSP**:

- OCSP needs a responder service and online reachability — a poor fit for a
  self-organizing LAN with no central server and dumb-node ambitions (D15).
- CRL is a signed list that must itself be distributed and refreshed — i.e. we'd
  reinvent gossip.

We already *have* a replicated, versioned, authenticated distribution channel: the
`ConfigDoc` over gossip ([07](./07-config-and-replication.md)). So:

> **Revocation = a `RevokedSet` (cert SHA-256 fingerprints) inside the `ConfigDoc` +
> short (30-day) cert lifetimes.** A forget adds the leaf's fingerprint; gossip pushes
> the new `ConfigDoc` to every full node within seconds; each node's mTLS verify
> callback (§5.4) rejects any peer whose cert fingerprint is in the set. The 30-day
> lifetime is the backstop: even a node that never receives the revocation (was
> offline, partitioned) loses trust when its leaf expires and it can no longer
> `/renew` (renew is mTLS-authenticated against the live cluster, §1.4 — a forgotten
> node's renew is rejected because its fingerprint is revoked and/or its `NodeRecord`
> is gone).

The `RevokedSet` schema (a monotonic, union-merged set of `RevokedCert{Fingerprint,
At}`) is owned by [07 §2.7](./07-config-and-replication.md); we key on the cert's
SHA-256 fingerprint rather than its serial so takeover — which reuses the node id with
a brand-new cert — only revokes the *specific* old certificate. Entries are retained
at least as long as the max cert lifetime (after which the cert is dead anyway).

### 5.3 Gossip rekey on forget closes the realtime hole

Removing `Addrs` from the allowlist stops the forgotten node *only if* it cannot
re-enter as a member. Because gossip membership re-feeds the live half of the
allowlist (§6.2), a forgotten node that still held the gossip key could re-announce
and slip its IP back in. Forget therefore **rekeys gossip** (reusing
`Membership.Rekey`, [02 §3.2](./02-cluster-discovery-membership.md)): the forgotten
node, lacking the new key, can no longer join, gossip, or get its IP re-added. Its
allowlist entry is gone and stays gone.

### 5.4 The mTLS verify hook

Both client- and server-side verification run the same extra check after chain
validation against the CA:

```go
// internal/pki — installed as tls.Config.VerifyPeerCertificate on BOTH sides.
func (v *PeerVerifier) Verify(rawCerts [][]byte, _ [][]*x509.Certificate) error {
    leaf := parse(rawCerts[0])
    // 1. chain to the cluster CA already enforced by ClientCAs/RootCAs + ClientAuth.
    // 2. reject if SHA-256(leaf DER) ∈ ConfigDoc.RevokedSet (live snapshot).
    if v.revoked(fingerprint(leaf)) { return ErrRevoked }
    // 3. (optional) confirm CN/URI-SAN matches a current NodeRecord id.
    return nil
}
```

---

## 6. The allowlist — the only guard on the realtime planes

### 6.1 What it is

`internal/allowlist` is a **source-IP gate** wrapping the clock and audio UDP sockets
(and the TCP-fallback audio listener). The spine ([README.md §6.5](./README.md)):

> *"The allowlist is derived from `Nodes[].Addrs` ∪ group membership: clock/audio
> sockets drop packets whose source IP is not a current cluster member's address."*

Because the clock plane (D6, [04](./04-clock-and-groups.md)) and the audio plane
(D5, [05](./05-audio-streaming-protocol.md)) carry **no authentication**, this gate is
the *entire* defense for those planes. It is a coarse, fail-safe filter: unknown
source ⇒ drop, silently.

### 6.2 Derivation (two sources, unioned, live)

```
 allowed-set  =  { IP  | IP ∈ NodeRecord.Addrs for any node in ConfigDoc.Nodes }   (durable, from 07)
              ∪  { IP  | IP advertised by a currently-alive gossip member }         (live, from 02 §3)
```

- The **durable** half comes from the replicated `ConfigDoc` (survives restarts,
  authoritative) — derivation owned by [07](./07-config-and-replication.md).
- The **live** half comes from memberlist `Members()` ([02 §3.3](./02-cluster-discovery-membership.md))
  to cover a member whose IP changed via DHCP before its `NodeRecord.Addrs` was
  updated. A forgotten node is in **neither** half once forget completes (§5.3).

### 6.3 Interface and dynamic update

```go
// internal/allowlist
type Set struct{ /* atomic snapshot of net.IP set */ }

// Allowed reports whether a source IP may send on the realtime planes.
func (s *Set) Allowed(ip net.IP) bool

// Update atomically swaps the snapshot; called whenever ConfigDoc.Nodes OR the live
// member set changes (both fan in via internal/state + internal/cluster.Changed()).
func (s *Set) Update(configAddrs, liveMemberAddrs []net.IP)

// GateUDP wraps a *net.UDPConn read loop: packets from disallowed sources are
// dropped before they reach the clock/audio decoders. The gate runs in the recv
// path so a rejected packet never touches the (untrusted-input) codec/FEC code.
func GateUDP(conn *net.UDPConn, set *Set, deliver func(src *net.UDPAddr, b []byte))
```

The gate re-reads its atomic snapshot per packet (lock-free). Update is driven by
the same `Changed()` signal the rest of the system uses
([02 §3.1](./02-cluster-discovery-membership.md)), so adopting a node opens its IP and
forgetting one closes it without a restart.

### 6.4 Honest limits

- **IP spoofing on the same L2 segment.** An attacker already on the LAN can forge a
  member's source IP in UDP and pass the gate. The allowlist does *not* defend
  against an on-link spoofer — only against off-path / non-member hosts. This is the
  accepted cost of unauthenticated realtime (D2/D5/D6); see §8 ("replay/spoofing on
  clock+audio"). Damage is bounded: a spoofed clock/audio packet can at worst perturb
  one group's playout (audible glitch / desync), never touch config or identity, and
  the stream's `streamGen`/`seq`/`sampleIndex` framing ([README.md §6.4](./README.md))
  makes stale/forged chunks easy for the receiver to reject as out-of-range.
- **No confidentiality.** Realtime audio is in clear on the LAN. Out of scope to
  encrypt (D2). An on-link attacker can record the audio stream.

---

## 7. UI / API authentication (D11)

Two **disjoint** authentication paths reach `/api/v1`. Keep them mentally separate:

```
                       ┌─────────────────────────── /api/v1 request ───────────────────────────┐
                       │                                                                        │
            ┌──────────┴───────────┐                                          ┌─────────────────┴───────────┐
            │   NODE path           │                                          │   HUMAN path                 │
            │   (peer ↔ peer)       │                                          │   (browser / script ↔ node)  │
            │   mTLS client cert    │                                          │   session cookie OR API key  │
            │   signed by cluster CA│                                          │   (admin password origin)    │
            │   ⇒ method:"node"     │                                          │   ⇒ method:"session"|"apiKey"│
            └───────────────────────┘                                          └──────────────────────────────┘
```

A request is authenticated if **either** path succeeds — exactly
[README.md §6.6](./README.md) / [08 §0.3](./08-http-api-reference.md).

### 7.1 Admin password (argon2id)

A **single cluster admin password** (D11; no user accounts — [README.md §1 non-goals](./README.md)).
Stored only as an **argon2id** hash in `ConfigDoc.Auth` (the spine's
`AuthConfig` "admin pw hash", [README.md §6.5](./README.md)):

```go
// internal/auth
type AdminCred struct {
    Argon2id string `json:"adminHash"` // PHC string: $argon2id$v=19$m=65536,t=3,p=2$...
}
func HashPassword(pw string) (phc string, err error)         // argon2id, per-hash random salt
func VerifyPassword(pw, phc string) bool                     // constant-time compare of the derived tag
```

Default params: `m=64MiB, t=3, p=2`. The PHC string is replicated in the
`ConfigDoc`, so any node can authenticate a login locally
([08 §B.2 Proxy: local](./08-http-api-reference.md)).

### 7.2 Sessions (cookies)

`POST /api/v1/login` ([08 §B.2](./08-http-api-reference.md)) verifies the password and
issues a session cookie:

| Cookie attribute | Value | Why |
|---|---|---|
| name | `ensemble_session` | matches [08 §B.2](./08-http-api-reference.md) |
| value | 32 random bytes, base64url | unguessable session id; server stores only its **hash** |
| `HttpOnly` | yes | JS cannot read it (XSS can't exfiltrate) |
| `SameSite` | `Strict` | CSRF defense (matches [08 §B.2](./08-http-api-reference.md)) |
| `Secure` | yes | cookie only over TLS — and the control plane is *always* TLS (mTLS, D10), so "Secure-over-mTLS" holds unconditionally |
| `Path` | `/` | whole API |
| TTL | 12 h sliding; absolute 7 d | bounded session lifetime |

Sessions are **per-node, in-memory** (not replicated): a cookie issued by N1 is not
valid at N2 — cross-node calls re-auth via mTLS or API key, never by forwarding a
cookie ([08 §B.2 Proxy](./08-http-api-reference.md)). Logout
([08 §B.3](./08-http-api-reference.md)) drops the server-side entry. This reuses the
mpvsync session shape but stores hashes, not plaintext.

```go
// internal/auth
type Sessions struct{ /* id-hash -> {issuedAt, lastSeen} */ }
func (s *Sessions) Issue() (cookieValue string)              // returns plaintext; stores hash
func (s *Sessions) Validate(cookieValue string) bool         // constant-time; slides TTL
func (s *Sessions) Revoke(cookieValue string)
```

### 7.3 API keys (revocable, hashed at rest)

Programmatic callers use `Authorization: Bearer <key>` (D11 "revocable API keys"):

- Minted via `POST /api/v1/apikeys` ([08 §B.6](./08-http-api-reference.md)); the
  plaintext secret (`ek_live_…`) is shown **exactly once**.
- Stored as a **hash** in `ConfigDoc.Auth` (the spine's "api key hashes",
  [README.md §6.5](./README.md)); the plaintext is never persisted. Because the hash
  is in the replicated doc, any node can verify a bearer key locally.
- Revoked via `DELETE /api/v1/apikeys/{id}` ([08 §B.7](./08-http-api-reference.md)) —
  drops the hash; revocation propagates by gossip
  ([07](./07-config-and-replication.md)).
- Hash choice: API keys are high-entropy (≥128-bit random), so a fast salted
  **SHA-256** with constant-time compare suffices (argon2id is for the *low-entropy*
  human password; an API key needs no memory-hardness). Stored with a per-key salt.

```go
// internal/auth
func NewAPIKey() (id, plaintext string)          // plaintext = "ek_live_" + base64url(32B)
func HashAPIKey(plaintext, salt string) string   // SHA-256(salt‖plaintext), hex
func VerifyAPIKey(plaintext string, stored []KeyRecord) (id string, ok bool) // constant-time
```

### 7.4 Middleware ordering

Every `/api/v1` handler is wrapped by one chain. **Order matters**; this is the
authoritative ordering:

```
incoming request
  │
  ├─[0] panic-recover + request log
  │
  ├─[1] UNINITIALIZED GATE
  │       if this node has no cluster (no CA / no admin hash):
  │         • allow ONLY: GET/POST /api/v1/setup  (§2.6) and  /bootstrap/*  (§9)
  │         • everything else  → 503 not_ready   ([08 §0.4])
  │       (this is why a raw node exposes only the setup wizard + bootstrap, [09 §1])
  │
  ├─[2] NODE PATH  — TLS client cert present AND chains to cluster CA AND
  │       fingerprint ∉ RevokedSet (§5.4)?
  │         → authenticated, method:"node"   → handler
  │
  ├─[3] HUMAN PATH — else require ONE of:
  │         • valid session cookie (§7.2)     → method:"session"
  │         • valid Bearer API key (§7.3)     → method:"apiKey"
  │       (login/logout/session are the bootstrap-of-the-human-path exceptions:
  │        /login is reachable with no session — it MINTS one; [08 §B.2])
  │
  ├─[4] PER-ENDPOINT AUTHZ — some calls are admin-session-only even though the
  │       request is authenticated (e.g. minting/listing API keys, [08 §B.5/B.6]
  │       require admin-session, NOT a node cert): enforce the endpoint's auth class.
  │
  ├─[5] If-Match precondition on config-mutating calls ([08 §0.5])
  │
  └─ handler
```

Rationale for the ordering:

- **[1] before everything**: an uninitialized node must not expose cluster surface it
  cannot secure (no CA ⇒ no mTLS ⇒ no node auth, and no admin hash ⇒ no human auth).
  The only doors are the setup wizard ([09 §1](./09-ui-screens.md)) and `/bootstrap/*`.
- **[2] node before human**: a peer call carries an mTLS cert; we authenticate it as a
  node first and skip session/key checks entirely. This cleanly realizes
  [README.md §6.6](./README.md)'s "mTLS client cert (node) OR session/API key".
- **[4] separate from [2/3]**: *authentication* (who are you) is distinct from
  *authorization* (may you do this). Key-management endpoints are interactive-admin
  only; a node cert authenticates but does not authorize them.

### 7.5 What's reachable when uninitialized (summary)

| Surface | Uninitialized | Initialized |
|---|---|---|
| `/bootstrap/info`, `/bootstrap/adopt` (§9) | **yes** (PIN-gated for adopt) | closed → `403` once a member ([08 §A.1](./08-http-api-reference.md)) |
| `GET/POST /api/v1/setup` | **yes** (the wizard) | `409 conflict` |
| everything else under `/api/v1` | `503 not_ready` | per auth class |

---

## 8. mTLS construction (D10)

Both directions are verified against the cluster CA: the dialing node presents a
**client** cert and verifies the server's; the listening node presents a **server**
cert and *requires+verifies* the client's. The same leaf (ExtKeyUsage
`ServerAuth+ClientAuth`, §1.3) serves both roles.

```go
// internal/pki — built from the live ConfigDoc + this node's leaf/key.
// caPool: from ConfigDoc.Cluster CA cert (public).  leaf: this node's cert+key.
// verifier: the revoked-set/CN check (§5.4), reading a live ConfigDoc snapshot.

func ServerTLS(leaf tls.Certificate, caPool *x509.CertPool, v *PeerVerifier) *tls.Config {
    return &tls.Config{
        MinVersion:            tls.VersionTLS13,
        Certificates:          []tls.Certificate{leaf},
        ClientAuth:            tls.RequireAndVerifyClientCert, // mTLS: client cert mandatory
        ClientCAs:             caPool,                         // verify client against cluster CA
        VerifyPeerCertificate: v.Verify,                       // + revoked-set (§5.4)
    }
}

func ClientTLS(leaf tls.Certificate, caPool *x509.CertPool, v *PeerVerifier) *tls.Config {
    return &tls.Config{
        MinVersion:            tls.VersionTLS13,
        Certificates:          []tls.Certificate{leaf},        // present OUR client cert
        RootCAs:               caPool,                         // verify server against cluster CA
        VerifyPeerCertificate: v.Verify,                       // + revoked-set (§5.4)
        // ServerName set to the target nodeId / IP so SAN matching (§1.3) holds.
    }
}
```

**How a node obtains the CA bundle + peers' certs.** The CA public cert arrives in
`ConfigDoc.Cluster` (during adoption out-of-band §2.4, thereafter via gossip merge,
[07](./07-config-and-replication.md)). Peers' **leaf** certs arrive in each
`NodeRecord.CertPEM` (the spine field "node's signed cert (public) — distributes
trust", [README.md §6.5](./README.md)) — but note mTLS does **not** require peers'
leaves to be pre-shared: TLS 1.3 sends the leaf in the handshake, and the only thing a
verifier needs is the **CA pool** + the **revoked-set**. `NodeRecord.CertPEM` is used
for (a) the revoked-set/CN cross-check (§5.4), (b) showing cert info in the UI
([09 §6](./09-ui-screens.md)), and (c) any explicit cert pinning. The `caPool` and
`PeerVerifier` are rebuilt whenever the `ConfigDoc` changes, so a freshly adopted CA
cert or a new revocation takes effect without restart.

The bootstrap surface (§9) is the **only** TLS endpoint that is *not* mTLS — it serves
a self-signed cert with `ClientAuth: NoClientCert`, because the adoptee has no
client cert yet.

---

## 9. Bootstrap / adoption endpoints (outside mTLS)

Authoritative request/response bodies: [08 §A](./08-http-api-reference.md). This
document specifies their *security semantics*; the table is the at-a-glance contract.

| Method & path | Auth | Body (carrier) | Purpose / this-doc section |
|---|---|---|---|
| `GET /bootstrap/info` | public-bootstrap (no PIN) | — → `{nodeId,name,fingerprint,state,protocolEpoch,softwareVersion,caps}` | probe + **fingerprint to pin** (§2.2) + **`protocolEpoch` to match** (§2.7, m7); `state` ∈ `uninitialized`/`foreign`/`member` ([02 §2.4](./02-cluster-discovery-membership.md)); `403` once a member |
| `POST /bootstrap/adopt?phase=key` | public-bootstrap + nonce | `{NB, nonceB}` → `{NA, nonceA}` | A.9 steps 2–3: swap ECDH publics + nonces; both derive `k`,`kp` (§2.3–2.4) |
| `POST /bootstrap/adopt?phase=csr` | public-bootstrap + **PIN-keyed tag** | `{csr_request, tag}` → `AEAD_k(csrPem)` | A.9 step 5: node verifies `tag`, returns ECDH-encrypted CSR (§2.3–2.4) |
| `POST /bootstrap/adopt?phase=complete` | public-bootstrap + **PIN-keyed tag2** | `{encPayload:AEAD_k(leafPem‖caBundlePem‖clusterSecrets), tag2, assignedNodeId, clusterName, seedPeers}` → `{nodeId,state:"member"}` | A.9 step 6: verify `tag2`, decrypt under ECDH key `k`, install signed leaf + CA + `ClusterSecrets` (`caKeyPEM`+`gossipKey`; persisted plaintext at rest, D18), become member (§2.4); takeover supersede (§4) |

> The `phase=key`/`phase=csr`/`phase=complete` split is this document's framing of the
> A.9 6-step exchange that [08 §A.2](./08-http-api-reference.md) describes as the
> bootstrap CSR/signed-artifact exchange. The single `POST /bootstrap/adopt` in
> [08](./08-http-api-reference.md) is realized as these phases; all live outside mTLS
> over the self-signed, fingerprint-pinned TLS channel, with confidentiality from the
> ECDH key `k` and authentication from the PIN-keyed HMAC tags.

Controller-side counterparts (inside mTLS / human auth), specified in
[08 §C](./08-http-api-reference.md): `POST /api/v1/adopt` (§2), `POST /api/v1/takeover`
(§4), `POST /api/v1/nodes/{id}/forget` (§5), plus the self-only
`POST /api/v1/nodes/{id}/renew` (§1.4).

---

## 10. Threat model

Honest accounting. "Residual" is what remains *after* the mitigation.

| # | Threat | Mitigation (this doc) | Residual risk |
|---|---|---|---|
| T1 | **Rogue node adoption** — attacker tries to join the cluster as a member | Adoption requires the PIN proof (§2.3) AND the operator must pick the node from Discovery and pin its fingerprint (§2.2). No CA-signed leaf ⇒ not a peer (§5.4, §8). | If the PIN is the `"0000"` default (0 effective entropy, §3.1) and the attacker is on-LAN, they can adopt *themselves* to the cluster by answering the handshake. **Real risk until per-device PINs (D9).** |
| T2 | **MITM of first contact** — intercept the bootstrap channel | Fingerprint-pin the self-signed cert (§2.2); ECDH gives confidentiality and the ECDH publics + nonces are bound into the PIN-keyed HMAC transcript so substitution breaks the tag (§2.5). | MITM that *also* knows the PIN can succeed; with `"0000"` that's trivial. Again per-device PIN / SPAKE2 (§3.5) closes it. |
| T3 | **Replay / spoofing on clock+audio** — inject/forge realtime UDP | Source-IP allowlist drops non-members (§6); receiver rejects out-of-range `streamGen`/`seq`/`sampleIndex` ([README.md §6.4](./README.md)). | **On-link IP spoofing is not stopped** (§6.4). Blast radius = transient audio glitch/desync for one group; never touches config/identity. Accepted cost of D2/D5/D6. |
| T4 | **Stolen API key** | Keys are revocable ([08 §B.7](./08-http-api-reference.md)), hashed at rest (§7.3), and carry no plaintext on the server. Revocation gossips cluster-wide in seconds. | A key is valid until noticed+revoked; it has full admin scope (single-admin model, [README.md §1](./README.md)). Mitigation is per-key labels/last-used ([08 §B.5](./08-http-api-reference.md)) to spot misuse. |
| T5 | **Stolen session cookie** | `HttpOnly`+`SameSite=Strict`+`Secure` (§7.2); per-node, bounded TTL; not replicated. | XSS in the bundled UI could still ride the session in-page; mitigated by it being `HttpOnly` (not exfiltratable) and short TTL. |
| T6 | **Lost / compromised CA key** | Model B + **D18**: any full node can sign; the CA key is **replicated in plaintext** (no sealing, §1.2). Recovery = **CA rotation** (§1.7): new CA + new `ClusterSecrets`, re-issue all leaves, rekey gossip, drop old. | **ACCEPTED under the limited LAN threat model (D18):** anyone with read access to a full node's disk gets the plaintext CA key (`ClusterSecrets.caKeyPEM`) and can mint certs → forge cluster membership. Acceptable for a trusted LAN; the mitigation is non-cryptographic (trust + physical control of full nodes) plus rotation after suspected compromise. **Revisit if the threat model widens** beyond a single trusted LAN (then: per-device sealing / HSM / Model C). |
| T7 | **Forgotten-node persistence** — a removed node keeps talking | Forget = revoked-set entry (control plane rejects it, §5.1/§5.4) **+** drop `Addrs` from allowlist (realtime drops it, §6) **+** gossip rekey (cannot re-join to re-add its IP, §5.3) **+** 30-day cert expiry backstop (§1.4). | A node that was forgotten while offline keeps a (revoked) cert until it next contacts the cluster (rejected) or it expires (≤30 days). On-link IP spoofing of a *still-allowed* member's IP is the §6.4 residual, unchanged by forget. |
| T8 | **Offline PIN capture** | ECDH keeps the CA bundle confidential against any capturer (§2.5); online throttle/lockout (§3.4); transcript binding (§2.3). | A passive capturer of one handshake can offline-brute the 4-digit PIN against the HMAC tags (§3.3) — confirming the PIN but **not** decrypting the secrets. Closed only by per-device PIN or SPAKE2 (§3.5). |
| T9 | **Downgrade / weak TLS** | TLS 1.3 only, AEAD suites, mTLS both ways (§8). | none of note for the control plane. |

The recurring honest theme: **the 4-digit `"0000"` PIN is the weakest link.** The
protocol never leaks it on the wire and binds it to the handshake (T2/T8 mitigations
are real), but ~13 bits — and *zero* for the placeholder — cannot withstand a
determined on-LAN attacker. The architecture is built so that raising PIN entropy
(per-device provisioning, D9) or swapping in SPAKE2 (§3.5) upgrades T1/T2/T8 with **no
protocol or endpoint changes** — that is the deliberate forward-compatibility this
document buys.
