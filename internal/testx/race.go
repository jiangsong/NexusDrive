// Package testx holds the few facts tests need about the build they run in.
package testx

// RaceEnabled reports whether the binary was built with -race. Wall-clock and
// throughput assertions are meaningless under the race detector, which slows
// CPU-bound code five to ten times; tests that make such assertions skip when
// this is true, the same way the standard library's own timing tests do.
const RaceEnabled = raceEnabled
