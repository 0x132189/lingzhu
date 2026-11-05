package datadir

import "testing"

func TestLockIsExclusiveAndReusable(t *testing.T) {
	dir := t.TempDir()

	first, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); err == nil {
		t.Fatal("a second core must not claim the same data directory")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	second, err := Lock(dir)
	if err != nil {
		t.Fatalf("lock should be reusable after release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseNilIsSafe(t *testing.T) {
	var guard *Guard
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
}
