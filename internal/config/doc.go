// Package config resolves the data-dir layout and holds Paths and Identity
// (node id, name, HWDelayUs); it parses config.yaml. Pure data + filesystem,
// no networking. Leaf package: imports no sibling internal packages.
package config
