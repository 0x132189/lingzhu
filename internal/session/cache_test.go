package session

import (
	"strings"
	"testing"

	"lingzhu/internal/event"
)

// The cache must stay inside its session budget, and a session evicted from it
// must still read back correctly from the database.
func TestCacheEvictsLeastRecentlyUsedSession(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.maxSize = 2
	store.maxByte = 0

	for _, id := range []string{"s1", "s2", "s3"} {
		if _, err := store.Append(id, event.User("hello "+id)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Events(id); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.cache) > 2 {
		t.Fatalf("cache holds %d sessions, want at most 2", len(store.cache))
	}
	if _, ok := store.cache["s1"]; ok {
		t.Fatal("the least recently used session should be evicted")
	}
	// A read after eviction reloads from the database and returns the same log.
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("unexpected reloaded log: %+v", events)
	}
}

// The byte budget also bounds the cache, so a few very large logs cannot grow
// it without limit.
func TestCacheEvictsPastByteBudget(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.maxSize = 100
	store.maxByte = 4 << 10

	for _, id := range []string{"s1", "s2", "s3", "s4"} {
		if _, err := store.Append(id, event.User(strings.Repeat("x", 2000))); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Events(id); err != nil {
			t.Fatal(err)
		}
	}
	if store.bytes > store.maxByte {
		t.Fatalf("cache holds %d bytes, want at most %d", store.bytes, store.maxByte)
	}
	if len(store.cache) == 0 {
		t.Fatal("the most recently used session should stay cached")
	}
}

// Reading a session must keep it resident while the others are evicted.
func TestCacheReadRefreshesRecency(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.maxSize = 2
	store.maxByte = 0

	for _, id := range []string{"s1", "s2"} {
		if _, err := store.Append(id, event.User("hello")); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Events(id); err != nil {
			t.Fatal(err)
		}
	}
	// Touch s1, then add s3: s2 becomes the oldest.
	if _, err := store.Events("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s3", event.User("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Events("s3"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.cache["s1"]; !ok {
		t.Fatal("a recently read session should stay cached")
	}
	if _, ok := store.cache["s2"]; ok {
		t.Fatal("the least recently used session should be evicted")
	}
}

// The byte accounting must return to zero once every session is evicted, so it
// cannot drift and evict everything forever.
func TestCacheByteAccountingBalances(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.maxSize = 1
	store.maxByte = 0

	for _, id := range []string{"s1", "s2", "s3"} {
		if _, err := store.Append(id, event.User("hello")); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Events(id); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.cache) != 1 {
		t.Fatalf("cache holds %d sessions, want 1", len(store.cache))
	}
	total := 0
	for _, events := range store.cache {
		total += cachedBytes(events)
	}
	if store.bytes != total {
		t.Fatalf("accounted bytes %d do not match cached bytes %d", store.bytes, total)
	}
}
