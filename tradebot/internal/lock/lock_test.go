//go:build unix

package lock

import "testing"

func TestExclusive(t *testing.T) {
	dir := t.TempDir()
	release, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir); err == nil {
		t.Fatal("second acquire must fail while the first is held")
	}
	release()
	r2, err := Acquire(dir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	r2()
}
