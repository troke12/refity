package registry

import "testing"

// A repository delete must not clear marks belonging to a repository whose name merely starts the same
// way, and must not clear anything outside blobs/.
func TestForgetCorruptPrefixIsPrecise(t *testing.T) {
	d := "sha256:" + "a" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde"
	cases := []struct {
		prefix string
		path   string
		want   bool // should this path be cleared?
	}{
		{"registry/grp/app/", "registry/grp/app/blobs/" + d, true},
		{"registry/grp/app/", "registry/grp/application/blobs/" + d, false},
		{"registry/grp/app/", "registry/grp/appx/blobs/" + d, false},
		{"registry/grp/", "registry/grp/app/blobs/" + d, true},
		{"registry/grp/", "registry/grp2/app/blobs/" + d, false},
		// A manifests entry can never be marked (markCorrupt rejects it), so it cannot be cleared
		// either; seeding it directly proves forgetCorruptPrefix does not depend on that.
		{"registry/grp/app/", "registry/grp/app/manifests/" + d, false},
	}
	for _, c := range cases {
		corrupt.Store(c.path, struct{}{})
		got := forgetCorruptPrefix(c.prefix) > 0
		if got != c.want {
			t.Errorf("prefix=%q path=%q cleared=%v want=%v", c.prefix, c.path, got, c.want)
		}
		corrupt.Range(func(k, _ any) bool { corrupt.Delete(k); return true })
	}
}

// A path that is not a blob object must never be markable: the mark outlives the process and is
// consulted by the upload path, so a manifests entry would pin an unrelated re-upload forever.
func TestMarkCorruptRejectsNonBlobPaths(t *testing.T) {
	newEnv(t, envOpts{})
	d := "sha256:" + strings64()
	for _, p := range []string{
		"registry/grp/app/manifests/" + d,
		"registry/grp/app/blobs/notadigest",
		"registry/grp/app/blobs/" + d + "/subdir",
		"just/a/path/" + d,
	} {
		markCorrupt(p)
		if knownCorrupt(p) {
			t.Errorf("%s was accepted as a corrupt mark", p)
		}
	}
	// And a real blob path is accepted, so the guard is not simply rejecting everything.
	good := "registry/grp/app/blobs/" + d
	markCorrupt(good)
	if !knownCorrupt(good) {
		t.Error("a genuine blob path was rejected")
	}
	clearCorrupt(good)
}
