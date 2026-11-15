package hypervisor

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNoKVMNamesTheWSLSettingOnlyUnderWSL(t *testing.T) {
	err := errors.New("no such file or directory")
	require.Contains(t, noKVM(err, true), "nestedVirtualization=true")
	require.Contains(t, noKVM(err, true), "wsl --shutdown")
	require.Equal(t, "KVM is not available (/dev/kvm: no such file or directory)", noKVM(err, false))
}
