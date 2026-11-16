package network

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTapAlreadyExists(t *testing.T) {
	// `ip tuntap add` asks for the device exclusively: an existing tap is
	// reported as busy. A VM started again after a daemon restart meets the
	// tap its predecessor left, and must take it over instead of coming up
	// without a network.
	for _, out := range []string{"ioctl(TUNSETIFF): Device or resource busy", "RTNETLINK answers: File exists"} {
		assert.True(t, tapAlreadyExists(out), out)
	}
	for _, out := range []string{"", "ioctl(TUNSETIFF): Operation not permitted", "open: No such file or directory", "ioctl(TUNSETIFF): Invalid argument"} {
		assert.False(t, tapAlreadyExists(out), out)
	}
}
