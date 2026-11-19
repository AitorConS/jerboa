//go:build !unix

package builder

// dirIdentity is empty where the platform gives no inode: the cache is
// keyed by the path alone.
func dirIdentity(string) string { return "" }
