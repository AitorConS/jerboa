//go:build !race && (linux || (darwin && arm64))

package vm

// raceDetector reports whether the tests run under the race detector.
const raceDetector = false
