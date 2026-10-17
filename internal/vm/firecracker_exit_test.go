package vm

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecordFCExitAfterGuestOutput(t *testing.T) {
	var log, attached bytes.Buffer
	log.WriteString("fio: Starting 4 threads\n")
	recordFCExit(&log, &attached, errors.New("HVF guest exit: RSS_LIMIT_EXCEEDED"))
	require.Contains(t, log.String(), "fio: Starting 4 threads")
	require.Contains(t, log.String(), "RSS_LIMIT_EXCEEDED")
	require.Contains(t, attached.String(), "RSS_LIMIT_EXCEEDED")
	before := log.String()
	recordFCExit(&log, nil, nil)
	require.Equal(t, before, log.String())
}
