package registry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"refity/backend/internal/driver/sftp"
)

// aliasDriver is a StorageDriver whose directory tree can be given aliases: two different paths that
// return the same children. SFTP has no symlinks, so this is the only way to exercise the audit walk's
// "already visited" branch — the case where a directory reachable twice would otherwise have its blobs
// hashed and reported twice.
type aliasDriver struct {
	mu     sync.Mutex
	names  map[string][]string
	lists  map[string]int
	blobs  map[string][]byte
	isDirs map[string]bool
}

func newAliasDriver() *aliasDriver {
	return &aliasDriver{
		names:  map[string][]string{},
		lists:  map[string]int{},
		blobs:  map[string][]byte{},
		isDirs: map[string]bool{},
	}
}

// addDir registers a directory with the given child names.
func (a *aliasDriver) addDir(path string, children ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.names[path] = children
	a.isDirs[path] = true
}

// aliasChild adds name under from, resolving to the same children as target. The alias lives at the full
// path (from + "/" + name) because that is the string the walk descends with.
func (a *aliasDriver) aliasChild(from, name, target string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.names[from] = append(a.names[from], name)
	a.names[from+"/"+name] = a.names[target]
	a.isDirs[from+"/"+name] = true
}

// selfAlias makes name under from resolve to from itself, so a walk that descends into it comes back.
func (a *aliasDriver) selfAlias(from, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.names[from] = append(a.names[from], name)
	// The alias must list from's children including the alias itself, or the walk stops after one hop
	// and never comes back.
	a.names[from+"/"+name] = append([]string{}, a.names[from]...)
	a.isDirs[from+"/"+name] = true
}

func (a *aliasDriver) addBlob(path string, data []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.blobs[path] = data
}

func (a *aliasDriver) listCount(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lists[path]
}

func (a *aliasDriver) List(_ context.Context, path string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lists[path]++
	if !a.isDirs[path] {
		return nil, fmt.Errorf("open %s: not a directory", path)
	}
	out := make([]string, len(a.names[path]))
	copy(out, a.names[path])
	return out, nil
}

func (a *aliasDriver) Stat(_ context.Context, path string) (sftp.FileInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	data, ok := a.blobs[path]
	if !ok {
		if a.isDirs[path] {
			return nil, fmt.Errorf("open %s: is a directory", path)
		}
		return nil, fmt.Errorf("open %s: no such file", path)
	}
	return aliasedFileInfo{size: int64(len(data))}, nil
}

func (a *aliasDriver) Reader(_ context.Context, path string, offset int64) (io.ReadCloser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	data, ok := a.blobs[path]
	if !ok {
		return nil, fmt.Errorf("open %s: no such file", path)
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	return io.NopCloser(bytesReader(data[offset:])), nil
}

func (a *aliasDriver) Delete(_ context.Context, path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.blobs, path)
	return nil
}

// The rest of StorageDriver is unused by the audit walk.
func (a *aliasDriver) Name() string                                      { return "alias" }
func (a *aliasDriver) RedirectURL(*http.Request, string) (string, error) { return "", nil }
func (a *aliasDriver) GetContent(context.Context, string) ([]byte, error) {
	return nil, nil
}
func (a *aliasDriver) PutContent(context.Context, string, []byte, ...func(int64, int64)) error {
	return nil
}
func (a *aliasDriver) Writer(context.Context, string, bool) (sftp.FileWriter, error) {
	return nil, fmt.Errorf("unused")
}
func (a *aliasDriver) Move(context.Context, string, string) error { return nil }
func (a *aliasDriver) Walk(context.Context, string, sftp.WalkFn, ...func(*sftp.WalkOptions)) error {
	return nil
}
func (a *aliasDriver) CreateRepositoryFolder(context.Context, string) error { return nil }
func (a *aliasDriver) DeleteRepositoryFolder(context.Context, string) error { return nil }
func (a *aliasDriver) CreateGroupFolder(context.Context, string) error      { return nil }

type aliasedFileInfo struct{ size int64 }

func (f aliasedFileInfo) Size() int64 { return f.size }

func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b []byte
	i int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
