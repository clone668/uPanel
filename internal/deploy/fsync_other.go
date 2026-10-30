//go:build !linux

package deploy

// Deployments only run on Linux. Other platforms compile the package for tests;
// Windows cannot fsync a directory handle.
func syncDirectory(string) error { return nil }
