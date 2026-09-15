//go:build darwin && !cgo

package vm

import "fmt"

func readDarwinUsage(int) (darwinUsage, error) {
	return darwinUsage{}, fmt.Errorf("native resource accounting requires a cgo build")
}

func listDarwinChildren(int) ([]int, error) {
	return nil, fmt.Errorf("native process tree accounting requires a cgo build")
}

const darwinStatsSource = "darwin-ps"
