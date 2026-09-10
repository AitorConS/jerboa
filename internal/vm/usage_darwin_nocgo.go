//go:build darwin && !cgo

package vm

import "fmt"

func readDarwinUsage(int) (darwinUsage, error) {
	return darwinUsage{}, fmt.Errorf("native resource accounting requires a cgo build")
}

const darwinStatsSource = "darwin-ps"
