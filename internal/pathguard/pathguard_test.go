package pathguard

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/myl7/file-edit-mcp/internal/errmsg"
)

// newGuard returns a guard over one fresh temp dir plus its canonical
// allowed root. On macOS, t.TempDir lives under /var/... which is itself a
// symlink to /private/var/...: New canonicalizes the allowed dir, so every
// in-bounds path in these tests must be built from the canonical root
// (AllowedDirs()[0]), not from the raw temp dir string. This mirrors
// production, where allowed dirs are realpath-resolved at startup (§3).
func newGuard(t *testing.T) (*Guard, string) {
	t.Helper()
	g, err := New([]string{t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g, g.AllowedDirs()[0]
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestNewCanonicalizesAllowedDirs(t *testing.T) {
	raw := t.TempDir()
	g, err := New([]string{raw})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want, err := filepath.EvalSymlinks(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := g.AllowedDirs()
	if len(got) != 1 || got[0] != want {
		t.Errorf("AllowedDirs() = %v, want [%s]", got, want)
	}
	// The returned slice must be a copy: mutating it may not affect the guard.
	got[0] = "/tampered"
	if g.AllowedDirs()[0] == "/tampered" {
		t.Error("AllowedDirs() leaked internal storage")
	}
}

func TestNewErrors(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Error("New(nil) succeeded, want error")
	}
	if _, err := New([]string{}); err == nil {
		t.Error("New([]) succeeded, want error")
	}
	if _, err := New([]string{filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("New with missing dir succeeded, want error")
	}
	file := filepath.Join(t.TempDir(), "plain.txt")
	writeFile(t, file, "x")
	if _, err := New([]string{file}); err == nil {
		t.Error("New with a file (not a dir) succeeded, want error")
	}
	// Duplicate inputs collapse to one canonical root.
	g, err := New([]string{file + "/..", filepath.Dir(file)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := len(g.AllowedDirs()); got != 1 {
		t.Errorf("dedupe: AllowedDirs() has %d entries, want 1", got)
	}
}

func TestValidateInBoundsFile(t *testing.T) {
	g, root := newGuard(t)
	writeFile(t, filepath.Join(root, "f.txt"), "hi")

	for _, p := range []string{
		filepath.Join(root, "f.txt"),
		root + "//f.txt",       // duplicate separator cleaned before comparison
		root + "/./f.txt",      // . cleaned
		root + "/sub/../f.txt", // .. cleaned back inside
		root + "/f.txt/",       // trailing separator on an existing file
	} {
		got, err := g.Validate(p)
		if err != nil {
			t.Errorf("Validate(%q): %v", p, err)
			continue
		}
		if got != filepath.Join(root, "f.txt") {
			t.Errorf("Validate(%q) = %q, want %q", p, got, filepath.Join(root, "f.txt"))
		}
	}
}

func TestValidateNulByte(t *testing.T) {
	g, root := newGuard(t)
	// Step order: the NUL byte wins even when later steps would also fire
	// (relative in step 4, drive shape in step 3).
	for _, p := range []string{
		"a\x00b",
		"\x00",
		"rel/..\x00/x",
		root + "/f\x00.txt",
		"C:\\a\x00b", // drive shape (step 3) must not outrank the NUL check (step 1)
	} {
		if _, err := g.Validate(p); !errors.Is(err, errmsg.ErrNulByte) {
			t.Errorf("Validate(%q) err = %v, want ErrNulByte", p, err)
		}
	}
}

func TestValidateTildeHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("UserHomeDir unavailable: %v", err)
	}
	g, _ := newGuard(t)

	// "~" resolves to the existing home dir; its realpath is outside the
	// allowed root → final EOutside from the step-7 gate.
	if _, err := g.Validate("~"); !errors.Is(err, errmsg.ErrOutside) {
		t.Errorf(`Validate("~") err = %v, want ErrOutside`, err)
	}

	// "~/x" expands to an absolute path outside the root. Whether ~/x
	// exists on this machine or not, the authoritative check lands outside:
	// existing → step 7 rejects the realpath; missing → step 8 rejects the
	// parent's (existing home's) realpath.
	_, err = g.Validate("~/x")
	if !errors.Is(err, errmsg.ErrOutside) {
		t.Errorf(`Validate("~/x") err = %v, want ErrOutside`, err)
	}
	if !strings.Contains(err.Error(), filepath.Clean(home)) {
		t.Errorf(`Validate("~/x"): message %q does not mention expanded home %q`, err.Error(), home)
	}

	// A clearly missing subdirectory under home: ENOENT branch, parent
	// (the subdirectory) does not exist → ErrParentMissing, still absolute.
	if _, err := g.Validate("~/.fem-no-such-dir-7f3c/f"); !errors.Is(err, errmsg.ErrParentMissing) {
		t.Errorf("Validate(~/.fem-no-such-dir-7f3c/f) err = %v, want ErrParentMissing", err)
	}
}

func TestValidateTildeUser(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skipf("user.Current unavailable: %v", err)
	}
	if _, err := user.Lookup(u.Username); err != nil {
		t.Skipf("user.Lookup(%q) unavailable: %v", u.Username, err)
	}
	g, _ := newGuard(t)

	// "~user/x" must expand to an absolute path, so it fails as ErrOutside
	// (not ErrRelative, which an unexpanded "~user/x" would produce).
	if _, err := g.Validate("~" + u.Username + "/x"); !errors.Is(err, errmsg.ErrOutside) {
		t.Errorf("Validate(~%s/x) err = %v, want ErrOutside", u.Username, err)
	}
	// Expansion failure falls through to the ordinary relative-path branch.
	if _, err := g.Validate("~no-such-user-zq/x"); !errors.Is(err, errmsg.ErrRelative) {
		t.Errorf("Validate(~no-such-user-zq/x) err = %v, want ErrRelative", err)
	}
}

func TestValidateWindowsDrive(t *testing.T) {
	g, _ := newGuard(t)
	for _, p := range []string{`C:\Users\x`, `c:/foo`, `Z:\`, `q:/`} {
		if _, err := g.Validate(p); !errors.Is(err, errmsg.ErrWindowsPath) {
			t.Errorf("Validate(%q) err = %v, want ErrWindowsPath", p, err)
		}
	}
	// "C:foo" does not match the ^[A-Za-z]:[\\/] shape; it is simply relative.
	if _, err := g.Validate("C:foo"); !errors.Is(err, errmsg.ErrRelative) {
		t.Errorf(`Validate("C:foo") err = %v, want ErrRelative`, err)
	}
}

func TestValidateRelative(t *testing.T) {
	g, root := newGuard(t)
	for _, p := range []string{"", "x", "./x", "sub/f.txt", "..", "../" + filepath.Base(root)} {
		if _, err := g.Validate(p); !errors.Is(err, errmsg.ErrRelative) {
			t.Errorf("Validate(%q) err = %v, want ErrRelative", p, err)
		}
	}
}

func TestValidateDotDotTraversal(t *testing.T) {
	g, root := newGuard(t)
	writeFile(t, filepath.Join(root, "b.txt"), "x")

	// Escapes the root: root/sub/../../escape cleans to a sibling path.
	if _, err := g.Validate(root + "/sub/../../escape"); !errors.Is(err, errmsg.ErrOutside) {
		t.Errorf("traversal escape err = %v, want ErrOutside", err)
	}
	// Climbs out and back in: cleans to the root itself.
	got, err := g.Validate(root + "/../" + filepath.Base(root))
	if err != nil || got != root {
		t.Errorf("climb-back Validate = (%q, %v), want (%q, nil)", got, err, root)
	}
	// .. inside an existing parent resolves to a new-file path.
	got, err = g.Validate(root + "/sub/../new.txt")
	if err != nil || got != filepath.Join(root, "new.txt") {
		t.Errorf("in-root .. Validate = (%q, %v), want (%q, nil)", got, err, filepath.Join(root, "new.txt"))
	}
}

func TestValidateSiblingPrefix(t *testing.T) {
	g, root := newGuard(t)
	// A path sharing root's string prefix but not its "/" boundary must be
	// rejected; this needs no filesystem access (fails at step 6).
	sibling := filepath.Join(filepath.Dir(root), filepath.Base(root)+"x")
	if _, err := g.Validate(sibling); !errors.Is(err, errmsg.ErrOutside) {
		t.Errorf("Validate(%q) err = %v, want ErrOutside", sibling, err)
	}
	if _, err := g.Validate("/"); !errors.Is(err, errmsg.ErrOutside) {
		t.Errorf(`Validate("/") err = %v, want ErrOutside`, err)
	}
}

func TestValidateSymlinkOutsideMidPathComponent(t *testing.T) {
	g, root := newGuard(t)
	outside := t.TempDir() // different tmp dir: guaranteed outside root
	writeFile(t, filepath.Join(outside, "f.txt"), "secret")
	if err := os.Symlink(outside, filepath.Join(root, "lnk")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The link sits before the final component; step 6 passes, step 7's
	// realpath gate catches it.
	if _, err := g.Validate(filepath.Join(root, "lnk", "f.txt")); !errors.Is(err, errmsg.ErrOutside) {
		t.Errorf("mid-component outside symlink err = %v, want ErrOutside", err)
	}
	// Non-existing file behind an outside link: ENOENT branch resolves the
	// parent (the link) and rejects at the parent prefix gate.
	if _, err := g.Validate(filepath.Join(root, "lnk", "new.txt")); !errors.Is(err, errmsg.ErrOutside) {
		t.Errorf("new file behind outside symlink err = %v, want ErrOutside", err)
	}
}

func TestValidateSymlinkOutsideFinalComponent(t *testing.T) {
	g, root := newGuard(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "f.txt")
	writeFile(t, target, "secret")
	if err := os.Symlink(target, filepath.Join(root, "flink")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The link IS the final component: step 6 passes, realpath of the link
	// points outside, step 7 rejects.
	if _, err := g.Validate(filepath.Join(root, "flink")); !errors.Is(err, errmsg.ErrOutside) {
		t.Errorf("final-component outside symlink err = %v, want ErrOutside", err)
	}
}

func TestValidateSymlinkInside(t *testing.T) {
	g, root := newGuard(t)
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(realDir, "f.txt"), "x")
	if err := os.Symlink(realDir, filepath.Join(root, "lnk")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Existing file via the link: the returned path must be the realpath
	// result (root/real/f.txt), not the symlinked spelling.
	got, err := g.Validate(filepath.Join(root, "lnk", "f.txt"))
	if err != nil {
		t.Fatalf("Validate via in-bounds link: %v", err)
	}
	if want := filepath.Join(realDir, "f.txt"); got != want {
		t.Errorf("resolved = %q, want realpath %q", got, want)
	}

	// New file via the link: ENOENT branch resolves one parent level, so
	// the result lands on the realpath side of the link too.
	got, err = g.Validate(filepath.Join(root, "lnk", "new.txt"))
	if err != nil {
		t.Fatalf("Validate new file via in-bounds link: %v", err)
	}
	if want := filepath.Join(realDir, "new.txt"); got != want {
		t.Errorf("resolved = %q, want %q", got, want)
	}
}

func TestValidateNewFileParentExists(t *testing.T) {
	g, root := newGuard(t)
	got, err := g.Validate(filepath.Join(root, "brand-new.txt"))
	if err != nil {
		t.Fatalf("new file with existing parent: %v", err)
	}
	if want := filepath.Join(root, "brand-new.txt"); got != want {
		t.Errorf("resolved = %q, want %q", got, want)
	}
}

func TestValidateParentMissing(t *testing.T) {
	g, root := newGuard(t)
	for _, p := range []string{
		filepath.Join(root, "nodir", "f.txt"),
		filepath.Join(root, "a", "b", "c.txt"), // only ONE parent level is checked
	} {
		if _, err := g.Validate(p); !errors.Is(err, errmsg.ErrParentMissing) {
			t.Errorf("Validate(%q) err = %v, want ErrParentMissing", p, err)
		}
	}
}

func TestValidateEqualsAllowedDir(t *testing.T) {
	g, root := newGuard(t)
	for _, p := range []string{root, root + "/", root + "/./"} {
		got, err := g.Validate(p)
		if err != nil || got != root {
			t.Errorf("Validate(%q) = (%q, %v), want (%q, nil)", p, got, err, root)
		}
	}
}

// TestValidatePathThroughFile: a path that descends "into" a plain file.
// EvalSymlinks returns ENOTDIR here (verified identical on macOS and
// Linux), which is a definite-class error per §8 and maps to ENotExist.
func TestValidatePathThroughFile(t *testing.T) {
	g, root := newGuard(t)
	writeFile(t, filepath.Join(root, "plain.txt"), "x")
	if _, err := g.Validate(filepath.Join(root, "plain.txt", "sub")); !errors.Is(err, errmsg.ErrNotExist) {
		t.Errorf("Validate(file/sub) err = %v, want ErrNotExist", err)
	}
}

func TestValidateMultipleAllowedDirs(t *testing.T) {
	rawA, rawB, rawC := t.TempDir(), t.TempDir(), t.TempDir()
	g, err := New([]string{rawA, rawB})
	if err != nil {
		t.Fatal(err)
	}
	dirA, dirB := g.AllowedDirs()[0], g.AllowedDirs()[1]

	writeFile(t, filepath.Join(dirA, "a.txt"), "a")
	writeFile(t, filepath.Join(dirB, "b.txt"), "b")
	if got, err := g.Validate(filepath.Join(dirA, "a.txt")); err != nil || got != filepath.Join(dirA, "a.txt") {
		t.Errorf("dirA file: (%q, %v)", got, err)
	}
	if got, err := g.Validate(filepath.Join(dirB, "b.txt")); err != nil || got != filepath.Join(dirB, "b.txt") {
		t.Errorf("dirB file: (%q, %v)", got, err)
	}

	_, err = g.Validate(filepath.Join(rawC, "c.txt"))
	if !errors.Is(err, errmsg.ErrOutside) {
		t.Errorf("outside file err = %v, want ErrOutside", err)
	}
	// EOutside must list every allowed directory.
	if !strings.Contains(err.Error(), dirA) || !strings.Contains(err.Error(), dirB) {
		t.Errorf("EOutside message %q does not list both allowed dirs", err.Error())
	}
}

// TestWithinRootBoundary locks the pure step-6 comparison, including the
// "/"-as-allowed-dir double-slash boundary (dir "/" must match "/tmp"
// even though "/" + "/" is "//", never a real path prefix).
func TestWithinRootBoundary(t *testing.T) {
	cases := []struct {
		dir, path string
		want      bool
	}{
		{"/", "/", true},
		{"/", "/tmp", true},
		{"/", "/tmp/x", true},
		{"/a", "/a", true},
		{"/a", "/a/b", true},
		{"/a", "/a/b/c", true},
		{"/a", "/ab", false},    // no separator boundary
		{"/a", "/a/../b", true}, // caller Cleans first; within is a pure byte comparison
		{"/a/b", "/a", false},
		{"/a", "rel", false}, // only absolute paths reach step 6
	}
	for _, tc := range cases {
		if got := within(tc.dir, tc.path); got != tc.want {
			t.Errorf("within(%q, %q) = %v, want %v", tc.dir, tc.path, got, tc.want)
		}
	}
}

// TestValidateSymlinkAliasAllowedDir reproduces the reported bug class:
// the guard stores the canonicalized (EvalSymlinks'd) allowed root, while
// the model sends a different spelling that reaches the same directory
// through a symlink alias. Under the revised §3 the cleaned form failing
// the fast path is not a veto; the realpath comparison decides.
func TestValidateSymlinkAliasAllowedDir(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(real, "a.txt"), "x")

	g, err := New([]string{real})
	if err != nil {
		t.Fatal(err)
	}
	root := g.AllowedDirs()[0] // canonical spelling

	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Existing file via the alias spelling: cleaned form matches no allowed
	// root, but the realpath lands inside → allowed, resolved to the real
	// spelling.
	got, err := g.Validate(filepath.Join(alias, "a.txt"))
	if err != nil {
		t.Fatalf("Validate via alias: %v", err)
	}
	if want := filepath.Join(root, "a.txt"); got != want {
		t.Errorf("alias access resolved = %q, want %q", got, want)
	}

	// New file via the alias: the ENOENT branch trusts the parent's
	// realpath (the alias resolves into the allowed root).
	got, err = g.Validate(filepath.Join(alias, "new.txt"))
	if err != nil {
		t.Fatalf("Validate new file via alias: %v", err)
	}
	if want := filepath.Join(root, "new.txt"); got != want {
		t.Errorf("alias new-file resolved = %q, want %q", got, want)
	}

	// Fail-closed neighbor: a path next to the alias still resolves outside
	// and must be rejected.
	if _, err := g.Validate(filepath.Join(base, "b.txt")); !errors.Is(err, errmsg.ErrOutside) {
		t.Errorf("Validate(%q) err = %v, want ErrOutside", filepath.Join(base, "b.txt"), err)
	}
}

// TestValidateSystemTmpAlias is the literal production repro: --allow over
// the canonical /private/tmp/<dir> while the request uses the /tmp/<dir>
// spelling (macOS). Skipped where /tmp is not a symlink (e.g. Linux).
func TestValidateSystemTmpAlias(t *testing.T) {
	realTmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil || realTmp == "/tmp" {
		t.Skipf("/tmp is not a symlink on this machine (realpath = %q, err = %v)", realTmp, err)
	}
	dir, err := os.MkdirTemp(realTmp, "fem-pathguard-*")
	if err != nil {
		t.Skipf("cannot create dir under %s: %v", realTmp, err)
	}
	defer os.RemoveAll(dir)
	writeFile(t, filepath.Join(dir, "a.txt"), "x")

	g, err := New([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	root := g.AllowedDirs()[0]
	if root != dir {
		t.Fatalf("New canonicalized to %q, want %q", root, dir)
	}

	alias := "/tmp/" + filepath.Base(dir)
	got, err := g.Validate(filepath.Join(alias, "a.txt"))
	if err != nil {
		t.Fatalf("Validate(%q): %v", filepath.Join(alias, "a.txt"), err)
	}
	if want := filepath.Join(root, "a.txt"); got != want {
		t.Errorf("resolved = %q, want %q", got, want)
	}

	got, err = g.Validate(filepath.Join(alias, "new.txt"))
	if err != nil {
		t.Fatalf("Validate new file via %q: %v", alias, err)
	}
	if want := filepath.Join(root, "new.txt"); got != want {
		t.Errorf("new-file resolved = %q, want %q", got, want)
	}
}

// TestValidateRootAllowedDir covers "/" as an allowed directory end to
// end. On macOS test machines EvalSymlinks("/")-anchored expectations are
// unreliable in sandboxed environments (everything folds to /private/...),
// so the case runs on Linux only; the "/" prefix logic itself is covered
// deterministically by TestWithinRootBoundary.
func TestValidateRootAllowedDir(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("allowed-dir=/ skipped on macOS: '/' realpath behavior differs (paths fold to /private/...); prefix logic covered by TestWithinRootBoundary")
	}
	g, err := New([]string{"/"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/etc", "/tmp"} {
		want, werr := filepath.EvalSymlinks(p)
		if werr != nil {
			continue
		}
		got, err := g.Validate(p)
		if err != nil || got != want {
			t.Errorf("Validate(%q) = (%q, %v), want (%q, nil)", p, got, err, want)
		}
	}
}
