package payload

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fixtureEntry describes one entry in a synthetic runner tarball.
type fixtureEntry struct {
	name     string
	mode     int64
	typeflag byte
	body     string
	linkname string
}

// fixtureEntries is the standard fake runner tarball: a run script, a
// helper binary, a diagnostics directory, and an in-tree symlink.
var fixtureEntries = []fixtureEntry{
	{name: "run.sh", mode: 0o755, typeflag: tar.TypeReg, body: "#!/bin/sh\nexit 0\n"},
	{name: "bin", mode: 0o755, typeflag: tar.TypeDir},
	{name: "bin/runhelper", mode: 0o755, typeflag: tar.TypeReg, body: "#!/bin/sh\necho helper\n"},
	{name: "bin/outer", mode: 0o755, typeflag: tar.TypeSymlink, linkname: "../run.sh"},
	{name: "_diag", mode: 0o755, typeflag: tar.TypeDir},
}

const fixtureVersion = "2.328.0"

// tarGzBytes builds a deterministic in-memory tar.gz from entries.
func tarGzBytes(t *testing.T, entries []fixtureEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	modTime := time.Unix(1700000000, 0)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Size:     int64(len(e.body)),
			Typeflag: e.typeflag,
			Linkname: e.linkname,
			ModTime:  modTime,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header for %s: %v", e.name, err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body for %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// fixtureBytes returns the standard fixture tarball.
func fixtureBytes(t *testing.T) []byte {
	t.Helper()
	return tarGzBytes(t, fixtureEntries)
}

// fixtureSHA256 returns the SHA-256 hex of the standard fixture tarball.
func fixtureSHA256(t *testing.T) string {
	t.Helper()
	return sha256Hex(fixtureBytes(t))
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// testEnv wires a Manager over temp dirs.
type testEnv struct {
	root        string
	cacheDir    string
	templateDir string
	m           *Manager
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	templateDir := filepath.Join(root, "template")
	return &testEnv{
		root:        root,
		cacheDir:    cacheDir,
		templateDir: templateDir,
		m:           New(cacheDir, templateDir, nil),
	}
}

// fixtureServer serves data and counts requests.
func fixtureServer(t *testing.T, data []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func cachePath(env *testEnv) string {
	return filepath.Join(env.cacheDir, fmt.Sprintf(cacheFileFmt, fixtureVersion))
}

// assertTemplate verifies the extracted template structure, modes, symlink,
// and marker.
func assertTemplate(t *testing.T, dir, version, sha string) {
	t.Helper()
	checkMode := func(rel string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("template entry %s: %v", rel, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %v, want %v", rel, got, want)
		}
	}
	checkMode("run.sh", 0o755)
	checkMode("bin", 0o755)
	checkMode("bin/runhelper", 0o755)
	info, err := os.Stat(filepath.Join(dir, "_diag"))
	if err != nil || !info.IsDir() {
		t.Fatalf("_diag: isDir=%v err=%v", info != nil && info.IsDir(), err)
	}
	link, err := os.Readlink(filepath.Join(dir, "bin", "outer"))
	if err != nil {
		t.Fatalf("readlink bin/outer: %v", err)
	}
	if link != "../run.sh" {
		t.Fatalf("bin/outer -> %q, want %q", link, "../run.sh")
	}
	v, s, ok, err := readMarker(filepath.Join(dir, markerName))
	if err != nil || !ok {
		t.Fatalf("marker: ok=%v err=%v", ok, err)
	}
	if v != version || s != sha {
		t.Fatalf("marker = (%q, %q), want (%q, %q)", v, s, version, sha)
	}
}

func TestDownloadURL(t *testing.T) {
	got := DownloadURL("2.328.0")
	want := "https://github.com/actions/runner/releases/download/v2.328.0/actions-runner-linux-x64-2.328.0.tar.gz"
	if got != want {
		t.Fatalf("DownloadURL() = %q, want %q", got, want)
	}
}

func TestResolveLatest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	serve := func(t *testing.T, body string, status int, wantAccept bool) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if wantAccept && !strings.Contains(r.Header.Get("Accept"), "vnd.github+json") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	release := func(tag, asset, digest string) string {
		assets := "[]"
		if asset != "" {
			assets = fmt.Sprintf(`[{"name": %q, "digest": %q}]`, asset, digest)
		}
		return fmt.Sprintf(`{"tag_name": %q, "assets": %s}`, tag, assets)
	}
	tarball := func(v string) string { return fmt.Sprintf("actions-runner-linux-x64-%s.tar.gz", v) }

	t.Run("resolves version and digest", func(t *testing.T) {
		srv := serve(t, release("v2.337.0", tarball("2.337.0"), digest), http.StatusOK, true)
		version, sha, err := ResolveLatest(context.Background(), srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if version != "2.337.0" {
			t.Fatalf("version = %q, want 2.337.0", version)
		}
		if sha != strings.Repeat("ab", 32) {
			t.Fatalf("sha = %q", sha)
		}
	})
	t.Run("accepts tag without v prefix", func(t *testing.T) {
		srv := serve(t, release("2.337.0", tarball("2.337.0"), digest), http.StatusOK, false)
		version, _, err := ResolveLatest(context.Background(), srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if version != "2.337.0" {
			t.Fatalf("version = %q", version)
		}
	})
	t.Run("rejects unusable tag", func(t *testing.T) {
		srv := serve(t, release("latest", tarball("latest"), digest), http.StatusOK, false)
		if _, _, err := ResolveLatest(context.Background(), srv.URL); err == nil {
			t.Fatal("expected error for unusable tag")
		}
	})
	t.Run("rejects missing linux asset", func(t *testing.T) {
		srv := serve(t, release("v2.337.0", "", digest), http.StatusOK, false)
		if _, _, err := ResolveLatest(context.Background(), srv.URL); err == nil {
			t.Fatal("expected error for missing linux-x64 asset")
		}
	})
	t.Run("rejects missing digest", func(t *testing.T) {
		srv := serve(t, release("v2.337.0", tarball("2.337.0"), ""), http.StatusOK, false)
		if _, _, err := ResolveLatest(context.Background(), srv.URL); err == nil {
			t.Fatal("expected error for missing digest")
		}
	})
	t.Run("rejects non-sha256 digest", func(t *testing.T) {
		srv := serve(t, release("v2.337.0", tarball("2.337.0"), "md5:abc"), http.StatusOK, false)
		if _, _, err := ResolveLatest(context.Background(), srv.URL); err == nil {
			t.Fatal("expected error for non-sha256 digest")
		}
	})
	t.Run("surfaces http errors", func(t *testing.T) {
		srv := serve(t, `{"message":"rate limited"}`, http.StatusForbidden, false)
		if _, _, err := ResolveLatest(context.Background(), srv.URL); err == nil {
			t.Fatal("expected error for non-200")
		}
	})
	t.Run("rejects malformed json", func(t *testing.T) {
		srv := serve(t, `{"tag_name":`, http.StatusOK, false)
		if _, _, err := ResolveLatest(context.Background(), srv.URL); err == nil {
			t.Fatal("expected error for malformed json")
		}
	})
}

func TestEnsureFreshDownload(t *testing.T) {
	env := newTestEnv(t)
	fixture := fixtureBytes(t)
	wantSHA := fixtureSHA256(t)
	srv, hits := fixtureServer(t, fixture)

	if err := env.m.Ensure(t.Context(), fixtureVersion, wantSHA, srv.URL); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1", hits.Load())
	}
	data, err := os.ReadFile(cachePath(env))
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if !bytes.Equal(data, fixture) {
		t.Fatalf("cached payload differs from fixture (%d vs %d bytes)", len(data), len(fixture))
	}
	assertTemplate(t, env.templateDir, fixtureVersion, wantSHA)
}

func TestEnsureCachedSkip(t *testing.T) {
	env := newTestEnv(t)
	fixture := fixtureBytes(t)
	wantSHA := fixtureSHA256(t)
	srv, hits := fixtureServer(t, fixture)

	if err := env.m.Ensure(t.Context(), fixtureVersion, wantSHA, srv.URL); err != nil {
		t.Fatalf("Ensure #1: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits after first Ensure = %d, want 1", hits.Load())
	}

	// Template present with matching marker: skip download and extract.
	if err := env.m.Ensure(t.Context(), fixtureVersion, wantSHA, srv.URL); err != nil {
		t.Fatalf("Ensure #2: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits after template skip = %d, want 1", hits.Load())
	}

	// Template gone but cache present: re-extract without downloading.
	if err := os.RemoveAll(env.templateDir); err != nil {
		t.Fatalf("remove template: %v", err)
	}
	if err := env.m.Ensure(t.Context(), fixtureVersion, wantSHA, srv.URL); err != nil {
		t.Fatalf("Ensure #3: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits after cache reuse = %d, want 1", hits.Load())
	}
	assertTemplate(t, env.templateDir, fixtureVersion, wantSHA)
}

func TestEnsureEvictsStaleTarballs(t *testing.T) {
	env := newTestEnv(t)
	fixture := fixtureBytes(t)
	wantSHA := fixtureSHA256(t)
	srv, hits := fixtureServer(t, fixture)

	if err := env.m.Ensure(t.Context(), fixtureVersion, wantSHA, srv.URL); err != nil {
		t.Fatalf("Ensure #1: %v", err)
	}

	stale := filepath.Join(env.cacheDir, "actions-runner-linux-x64-0.0.0.tar.gz")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(env.cacheDir, "actions-runner-linux-x64-9.9.9.tar.gz.1234.tmp")
	if err := os.WriteFile(staging, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(env.cacheDir, "operator-notes.txt")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The template already matches, so Ensure returns before any download;
	// eviction must still run on this early-return path.
	if err := env.m.Ensure(t.Context(), fixtureVersion, wantSHA, srv.URL); err != nil {
		t.Fatalf("Ensure #2: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1", hits.Load())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale tarball after Ensure: stat err = %v, want not exist", err)
	}
	for _, keep := range []string{cachePath(env), staging, unrelated} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("expected %s to survive eviction: %v", keep, err)
		}
	}
}

func TestEnsureShaMismatch(t *testing.T) {
	env := newTestEnv(t)
	fixture := fixtureBytes(t)
	wrongSHA := sha256Hex([]byte("not the payload"))
	srv, _ := fixtureServer(t, fixture)

	err := env.m.Ensure(t.Context(), fixtureVersion, wrongSHA, srv.URL)
	if err == nil {
		t.Fatal("Ensure with wrong sha: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("error %q does not mention checksum", err)
	}
	// The poisoned cache must be evicted and no template materialized.
	if _, err := os.Stat(cachePath(env)); !os.IsNotExist(err) {
		t.Fatalf("cache file still present after mismatch; stat err = %v", err)
	}
	if _, err := os.Stat(env.templateDir); !os.IsNotExist(err) {
		t.Fatalf("template dir exists after failed Ensure; stat err = %v", err)
	}
	if _, err := os.Stat(env.templateDir + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("staging dir left behind; stat err = %v", err)
	}
}

func TestEnsureMissingSHA(t *testing.T) {
	fixture := fixtureBytes(t)
	for _, tc := range []struct {
		name    string
		envVal  string // value for AllowUnverifiedEnv; "" = explicit unset
		wantErr bool
	}{
		{name: "unset", wantErr: true},
		{name: "zero", envVal: "0", wantErr: true},
		{name: "allowed", envVal: "1", wantErr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(AllowUnverifiedEnv, tc.envVal)
			env := newTestEnv(t)
			srv, hits := fixtureServer(t, fixture)
			err := env.m.Ensure(t.Context(), fixtureVersion, "", srv.URL)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Ensure with empty sha: expected error, got nil")
				}
				if hits.Load() != 0 {
					t.Fatalf("server hits = %d, want 0 (must fail before download)", hits.Load())
				}
				return
			}
			if err != nil {
				t.Fatalf("Ensure with allow-unverified: %v", err)
			}
			if hits.Load() != 1 {
				t.Fatalf("server hits = %d, want 1", hits.Load())
			}
			assertTemplate(t, env.templateDir, fixtureVersion, "")
		})
	}
}

func TestEnsureTraversalRejected(t *testing.T) {
	env := newTestEnv(t)
	evil := make([]fixtureEntry, 0, len(fixtureEntries)+1)
	evil = append(evil, fixtureEntries...)
	evil = append(evil, fixtureEntry{name: "../escape", mode: 0o755, typeflag: tar.TypeReg, body: "boom"})
	data := tarGzBytes(t, evil)
	wantSHA := sha256Hex(data)
	srv, _ := fixtureServer(t, data)

	err := env.m.Ensure(t.Context(), fixtureVersion, wantSHA, srv.URL)
	if err == nil {
		t.Fatal("Ensure with traversal entry: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("error %q does not mention unsafe entry", err)
	}
	if _, err := os.Stat(env.templateDir + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("staging dir left behind; stat err = %v", err)
	}
	if _, err := os.Stat(env.templateDir); !os.IsNotExist(err) {
		t.Fatalf("template dir exists after failed extract; stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.root, "escape")); !os.IsNotExist(err) {
		t.Fatalf("traversal wrote outside extraction root; stat err = %v", err)
	}
}

func TestEnsureNoTmpLeftOnSuccess(t *testing.T) {
	env := newTestEnv(t)
	fixture := fixtureBytes(t)
	wantSHA := fixtureSHA256(t)
	srv, _ := fixtureServer(t, fixture)

	if err := env.m.Ensure(t.Context(), fixtureVersion, wantSHA, srv.URL); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	err := filepath.WalkDir(env.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".tmp") {
			return fmt.Errorf("stale temp path %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("leftover *.tmp under %s: %v", env.root, err)
	}
}

func TestCopySlot(t *testing.T) {
	env := newTestEnv(t)
	fixture := fixtureBytes(t)
	wantSHA := fixtureSHA256(t)
	srv, _ := fixtureServer(t, fixture)
	if err := env.m.Ensure(t.Context(), fixtureVersion, wantSHA, srv.URL); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	slotDir := filepath.Join(env.root, "slots", "0001")
	if err := os.MkdirAll(filepath.Dir(slotDir), 0o755); err != nil {
		t.Fatalf("mkdir slots: %v", err)
	}
	if err := env.m.CopySlot(slotDir); err != nil {
		t.Fatalf("CopySlot: %v", err)
	}
	checkMode := func(rel string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(filepath.Join(slotDir, rel))
		if err != nil {
			t.Fatalf("slot %s: %v", rel, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("slot %s mode = %v, want %v", rel, got, want)
		}
	}
	checkMode("run.sh", 0o755)
	checkMode("bin", 0o755)
	checkMode("bin/runhelper", 0o755)
	info, err := os.Stat(filepath.Join(slotDir, "_diag"))
	if err != nil || !info.IsDir() {
		t.Fatalf("slot _diag: isDir=%v err=%v", info != nil && info.IsDir(), err)
	}
	link, err := os.Readlink(filepath.Join(slotDir, "bin", "outer"))
	if err != nil {
		t.Fatalf("slot readlink bin/outer: %v", err)
	}
	if link != "../run.sh" {
		t.Fatalf("slot bin/outer -> %q, want %q", link, "../run.sh")
	}
	data, err := os.ReadFile(filepath.Join(slotDir, "bin", "runhelper"))
	if err != nil || string(data) != "#!/bin/sh\necho helper\n" {
		t.Fatalf("slot runhelper content %q, err %v", data, err)
	}

	// Refuse an existing destination.
	if err := env.m.CopySlot(slotDir); err == nil {
		t.Fatal("CopySlot onto existing dir: expected error, got nil")
	}
	// Missing parent is an error.
	if err := env.m.CopySlot(filepath.Join(env.root, "missing-parent", "0002")); err == nil {
		t.Fatal("CopySlot with missing parent: expected error, got nil")
	}
	// Template not ready is an error.
	emptyEnv := newTestEnv(t)
	emptySlot := filepath.Join(emptyEnv.root, "slots", "0001")
	if err := os.MkdirAll(filepath.Dir(emptySlot), 0o755); err != nil {
		t.Fatalf("mkdir slots: %v", err)
	}
	if err := emptyEnv.m.CopySlot(emptySlot); err == nil {
		t.Fatal("CopySlot without template: expected error, got nil")
	}
}

func TestCopyTreeCancellation(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "slot")
	if err := os.WriteFile(filepath.Join(src, "payload"), []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyTreeContext(ctx, src, dst); !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("cancelled copy created destination: %v", err)
	}
}
