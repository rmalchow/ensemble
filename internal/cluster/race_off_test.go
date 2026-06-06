//go:build !race

package cluster

// raceEnabled reports whether the test binary was built with -race. The live
// two-node memberlist integration tests are skipped under -race: memberlist's
// Members() returns pointers into node structs that its own gossip goroutines
// mutate in place (memberlist@v0.5.4 state.go aliveNode vs our field reads), an
// upstream API-level data race we cannot synchronize from the consumer side.
// The pure-logic tests (election, peers, delegate, Meta) stay -race-clean and
// fully cover this package's own concurrency.
const raceEnabled = false
