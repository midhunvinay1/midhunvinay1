//go:build !unix

package lock

// Acquire is a no-op on platforms without flock; run one process at a time.
func Acquire(string) (func(), error) { return func() {}, nil }
