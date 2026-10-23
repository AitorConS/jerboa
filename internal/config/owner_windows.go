package config

// checkOwner is a no-op on Windows, where the temp directory is per user.
func checkOwner(string) error { return nil }
