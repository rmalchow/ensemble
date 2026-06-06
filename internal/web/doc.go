// Package web holds the HTTP handlers (mTLS), the Deps function-value seam, and
// the embedded UI assets. It must never import group, stream/*, or audio/* —
// the engine is reached only through the Deps seam.
package web
