//go:build !linux && !darwin

package builder

func confine(Step, string, []string, string, []string) ([]string, bool) { return nil, false }

func sandboxName() string { return "" }
