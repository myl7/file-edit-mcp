package cifsops

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// writeFile is a small helper so tests read as the production layer would
// (Root-relative names, never absolute ones inside Root calls).
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRootPoolLazyOpenAndCache(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "hello")
	pool := NewRootPool([]string{dir})

	if len(pool.roots) != 0 {
		t.Fatalf("fresh pool opened %d roots, want 0 (lazy)", len(pool.roots))
	}

	r1, err := pool.Open(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.roots) != 1 {
		t.Fatalf("after first Open, cached roots = %d, want 1", len(pool.roots))
	}
	r2, err := pool.Open(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 {
		t.Error("Open returned different Root handles, want the cached one")
	}

	data, err := r1.ReadFile("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("Root.ReadFile = %q, want %q", data, "hello")
	}
}

func TestRootPoolResolveRelNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "a", "b", "c.txt"), "x")
	pool := NewRootPool([]string{dir})

	root, rel, err := pool.Resolve(filepath.Join(dir, "a", "b", "c.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if rel != "a/b/c.txt" {
		t.Errorf("Resolve rel = %q, want %q", rel, "a/b/c.txt")
	}
	if _, err := root.Stat(rel); err != nil {
		t.Errorf("Stat(%q) = %v, want nil", rel, err)
	}

	rootSelf, relSelf, err := pool.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if relSelf != "." || rootSelf != root {
		t.Errorf("Resolve(dir) = (%p, %q), want (same root, \".\")", rootSelf, relSelf)
	}
}

func TestRootPoolOpenOutsideErrors(t *testing.T) {
	dir := t.TempDir()
	sibling := filepath.Join(filepath.Dir(dir), "elsewhere-"+filepath.Base(dir))
	pool := NewRootPool([]string{dir})

	if _, err := pool.Open(sibling); err == nil {
		t.Errorf("Open(%q) succeeded, want error (outside pool)", sibling)
	}
	if _, _, err := pool.Resolve("/definitely/not/pooled"); err == nil {
		t.Error("Resolve(absolute outside) succeeded, want error")
	}
}

func TestRootPoolNestedDirsLongestPrefix(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sub, "f.txt"), "in-sub")
	pool := NewRootPool([]string{dir, sub})

	root, rel, err := pool.Resolve(filepath.Join(sub, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if rel != "f.txt" {
		t.Errorf("rel = %q, want %q (tightest root wins)", rel, "f.txt")
	}
	if root.Name() == dir || root.Name() == "" {
		// Name() reports the directory the Root was opened on.
		t.Errorf("Root.Name() = %q, want the nested dir %q", root.Name(), sub)
	}
}

func TestRootPoolReopenAll(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "v1")
	pool := NewRootPool([]string{dir})

	r1, rel, err := pool.Resolve(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.ReopenAll(); err != nil {
		t.Fatalf("ReopenAll = %v, want nil", err)
	}

	r2, rel2, err := pool.Resolve(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if r1 == r2 {
		t.Fatal("ReopenAll kept the old Root handle, want a fresh one")
	}
	if rel2 != rel {
		t.Errorf("rel changed across reopen: %q -> %q", rel, rel2)
	}

	// The fresh Root works; the closed one errors.
	data, err := r2.ReadFile(rel2)
	if err != nil {
		t.Fatalf("fresh Root ReadFile = %v, want nil", err)
	}
	if string(data) != "v1" {
		t.Errorf("fresh Root ReadFile = %q, want %q", data, "v1")
	}
	if _, err := r1.ReadFile(rel); err == nil {
		t.Error("old Root still usable after Close, want an error")
	}
}

func TestRootPoolReopenAllWithoutOpenedRoots(t *testing.T) {
	dir := t.TempDir()
	pool := NewRootPool([]string{dir})
	if err := pool.ReopenAll(); err != nil {
		t.Fatalf("ReopenAll on a fully lazy pool = %v, want nil", err)
	}
	if len(pool.roots) != 0 {
		t.Errorf("ReopenAll eagerly opened %d roots, want 0 (lazy)", len(pool.roots))
	}
}

func TestRootPoolReopenAllMissingDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "x")
	pool := NewRootPool([]string{dir})
	if _, _, err := pool.Resolve(filepath.Join(dir, "f.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	err := pool.ReopenAll()
	if err == nil {
		t.Fatal("ReopenAll succeeded on a deleted directory, want error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReopenAll err = %v, want an ENOENT-bearing error", err)
	}
}

func TestRootPoolConcurrentOpen(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f.txt"), "shared")
	pool := NewRootPool([]string{dir})

	var wg sync.WaitGroup
	roots := make([]*os.Root, 16)
	for i := range roots {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			root, rel, err := pool.Resolve(filepath.Join(dir, "f.txt"))
			if err != nil {
				t.Errorf("Resolve = %v", err)
				return
			}
			if _, err := root.ReadFile(rel); err != nil {
				t.Errorf("ReadFile = %v", err)
			}
			roots[i] = root
		}(i)
	}
	wg.Wait()

	for i, root := range roots {
		if root != nil && root != roots[0] {
			t.Errorf("goroutine %d got a different Root handle, want one shared cache entry", i)
		}
	}
}
