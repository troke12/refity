package registry

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Concurrent marking, clearing and reloading must never lose a mark that is still set, and the file on
// disk must always agree with memory (never a phantom that reloads as damage).
func TestCorruptMarksSurviveConcurrentTraffic(t *testing.T) {
	// Use this env's own spool directory: the package-level spoolDir may still belong to an earlier env,
	// and writing to it would leave the store unbound (every persist silently dropped) and pollute
	// whatever test runs next.
	env := newEnv(t, envOpts{})
	store := filepath.Join(env.spool, corruptFileName)
	d := "sha256:"

	// Two disjoint sets, so no path is touched by two goroutines: even paths are marked and must stay,
	// odd paths are marked and immediately cleared. Overlapping sets would race by construction and the
	// assertion could not tell a lost mark from an expected clear.
	var keep, clear []string
	for i := 0; i < 40; i++ {
		p := "registry/grp/app/blobs/" + d + strings64()[:62] + hexPair(i)
		if i%2 == 0 {
			keep = append(keep, p)
		} else {
			clear = append(clear, p)
		}
	}

	var wg sync.WaitGroup
	for _, p := range keep {
		wg.Add(1)
		go func(p string) { defer wg.Done(); markCorrupt(p) }(p)
	}
	for _, p := range clear {
		wg.Add(1)
		go func(p string) { defer wg.Done(); markCorrupt(p); clearCorrupt(p) }(p)
	}
	wg.Wait()

	for _, p := range keep {
		if !knownCorrupt(p) {
			t.Fatalf("%s was lost under concurrent marking", p)
		}
	}
	for _, p := range clear {
		if knownCorrupt(p) {
			t.Fatalf("%s was cleared but is still marked", p)
		}
	}

	// And the file must agree with memory after a reload: this is what a restart would see.
	corrupt.Range(func(k, _ any) bool { corrupt.Delete(k); return true })
	corruptRemovedAt.Range(func(k, _ any) bool { corruptRemovedAt.Delete(k); return true })
	corruptState.load()
	for _, p := range keep {
		if !knownCorrupt(p) {
			t.Fatalf("after reload %s is gone; the file on disk did not carry every outstanding mark", p)
		}
	}
	for _, p := range clear {
		if knownCorrupt(p) {
			t.Fatalf("after reload %s reappeared; a cleared mark was persisted", p)
		}
	}
	if _, err := os.Stat(store); err != nil {
		t.Fatalf("mark file missing: %v", err)
	}
}

func hexPair(i int) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[(i/16)%16], hex[i%16]})
}
