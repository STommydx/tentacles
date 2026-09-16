// Package payload manages the official actions/runner tarball: download,
// SHA-256 verification, and extraction into a reusable template directory
// that runner slots are copied from.
package payload

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// AllowUnverifiedEnv is the environment variable that permits a payload
// without a sha256 checksum. Set it to "1" (or true/yes/on) to bypass
// verification; never set it in production.
const AllowUnverifiedEnv = "TENTACLES_ALLOW_UNVERIFIED_PAYLOAD"

const (
	// cacheFileFmt is the cache file name for a given runner version.
	cacheFileFmt = "actions-runner-linux-x64-%s.tar.gz"
	// payloadPrefix identifies cached payload tarballs in cacheDir;
	// only files with this prefix and a .tar.gz suffix are eviction
	// candidates. Staging files (.tmp) and unrelated files are left alone.
	payloadPrefix = "actions-runner-linux-x64-"
	// markerName identifies a successfully extracted template directory.
	markerName = ".tentacles-template"
	// maxDownloadSize caps the tarball body size (1 GiB).
	maxDownloadSize = 1 << 30
	// defaultHTTPTimeout bounds a single download request. The tarball is
	// large and hosts can be slow, so this is generous; callers can bound
	// the overall operation with the context passed to Ensure.
	defaultHTTPTimeout = 30 * time.Minute
)

// resolveHTTPTimeout bounds a single releases-API call.
const resolveHTTPTimeout = 30 * time.Second

// versionShape matches the bare X.Y.Z versions the config contract and
// the release asset names are built from.
var versionShape = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// ReleasesAPIURL is the GitHub API endpoint ResolveLatest queries when
// the config leaves runner.version unset.
const ReleasesAPIURL = "https://api.github.com/repos/actions/runner/releases/latest"

// releaseResponse and releaseAsset are the fields of the GitHub
// releases API response this daemon consumes.
type releaseResponse struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// ResolveLatest queries the GitHub releases API for the latest
// actions/runner release and returns the version (tag without the "v")
// and the sha256 of the linux-x64 tarball taken from the release asset
// digest. It fails closed when the release carries no matching asset or
// no usable digest; in that case pin runner.version and runner.sha256
// instead of running unverified.
func ResolveLatest(ctx context.Context, apiURL string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("payload: build releases request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: resolveHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("payload: query releases API %s: %w", apiURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("payload: releases API %s: unexpected status %s", apiURL, resp.Status)
	}
	var rel releaseResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", "", fmt.Errorf("payload: decode releases response: %w", err)
	}
	version := strings.TrimPrefix(rel.TagName, "v")
	if !versionShape.MatchString(version) {
		return "", "", fmt.Errorf("payload: releases API returned unusable tag %q", rel.TagName)
	}
	want := fmt.Sprintf(cacheFileFmt, version)
	for _, a := range rel.Assets {
		if a.Name != want {
			continue
		}
		sha, ok := strings.CutPrefix(a.Digest, "sha256:")
		if !ok || checkSHA256(sha) != nil {
			return "", "", fmt.Errorf("payload: asset %s carries unusable digest %q; pin runner.version and runner.sha256 instead", a.Name, a.Digest)
		}
		return version, sha, nil
	}
	return "", "", fmt.Errorf("payload: release %s has no %s asset; pin runner.version and runner.sha256 instead", rel.TagName, want)
}

// Manager downloads, verifies, and extracts the official runner tarball.
// Ensure is safe for concurrent use; CopySlot may run while a different
// version is being swapped in and copies one complete template.
type Manager struct {
	mu          chan struct{}
	cacheDir    string
	templateDir string
	log         *slog.Logger
	httpClient  *http.Client
}

// New returns a Manager that caches tarballs in cacheDir and extracts them
// into templateDir. A nil log is replaced with a discard logger.
func New(cacheDir, templateDir string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{
		mu:          make(chan struct{}, 1),
		cacheDir:    filepath.Clean(cacheDir),
		templateDir: filepath.Clean(templateDir),
		log:         log,
		httpClient:  &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// DownloadURL returns the canonical GitHub release URL for a runner version.
func DownloadURL(version string) string {
	return "https://github.com/actions/runner/releases/download/v" + version +
		"/actions-runner-linux-x64-" + version + ".tar.gz"
}

// Ensure makes sure the payload for version is cached, verified, and
// extracted into the template directory. When the template already matches
// version+sha256, download and extraction are skipped entirely. Tarballs
// for other versions are evicted so cache_dir holds at most the current
// one. An empty sha256 is an error unless AllowUnverifiedEnv is set.
func (m *Manager) Ensure(ctx context.Context, version, sha256, downloadURL string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.mu <- struct{}{}:
		defer func() { <-m.mu }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if !safeComponent(version) {
		return fmt.Errorf("payload: invalid version %q", version)
	}
	if err := checkSHA256(sha256); err != nil {
		return err
	}
	m.evictStaleTarballs(version)

	if m.templateMatches(version, sha256) {
		m.log.Info("payload template already materialized", "version", version, "template", m.templateDir)
		return nil
	}

	cachePath := m.cachePath(version)
	if !cacheUsable(cachePath) {
		if err := m.download(ctx, downloadURL, cachePath, version); err != nil {
			return err
		}
	}
	for attempt := range 2 {
		ok, err := m.verify(ctx, cachePath, sha256)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if ok {
			break
		}
		if attempt == 1 {
			// Evict the poisoned cache so a later Ensure re-downloads.
			_ = os.Remove(cachePath)
			return fmt.Errorf("payload: checksum mismatch for %s after redownload", cachePath)
		}
		m.log.Warn("payload checksum mismatch; discarding cache and redownloading", "path", cachePath)
		if err := os.Remove(cachePath); err != nil {
			return fmt.Errorf("payload: remove corrupt cache %s: %w", cachePath, err)
		}
		if err := m.download(ctx, downloadURL, cachePath, version); err != nil {
			return err
		}
	}
	return m.extract(ctx, cachePath, version, sha256)
}

// CopySlot copies the template directory into dst (like cp -a), preserving
// modes and symlinks. dst must not exist; its parent must exist.
func (m *Manager) CopySlot(dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("payload: slot directory %s already exists", dst)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("payload: stat %s: %w", dst, err)
	}
	parent := filepath.Dir(dst)
	if _, err := os.Stat(parent); err != nil {
		return fmt.Errorf("payload: slot parent %s: %w", parent, err)
	}
	info, err := os.Stat(m.templateDir)
	if err != nil {
		return fmt.Errorf("payload: template %s not ready: %w", m.templateDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("payload: template %s is not a directory", m.templateDir)
	}
	m.log.Info("copying payload template to slot", "from", m.templateDir, "to", dst)
	if err := copyTree(m.templateDir, dst); err != nil {
		return fmt.Errorf("payload: copy template to %s: %w", dst, err)
	}
	m.log.Info("slot materialized", "dir", dst)
	return nil
}

// CopySlotIntoContext fills an empty directory exclusively reserved by the
// table. It never accepts a symlink or a previously populated directory.
func (m *Manager) CopySlotIntoContext(ctx context.Context, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(dst)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("payload: reserved slot is not a directory: %s", dst)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("payload: reserved slot is not empty: %s", dst)
	}
	return copyTreeContext(ctx, m.templateDir, dst)
}

// cachePath returns the cache file path for a runner version.
func (m *Manager) cachePath(version string) string {
	return filepath.Join(m.cacheDir, fmt.Sprintf(cacheFileFmt, version))
}

// templateMatches reports whether the template directory is already
// materialized for version+sha256: non-empty and carrying a matching marker.
func (m *Manager) templateMatches(version, sha256 string) bool {
	entries, err := os.ReadDir(m.templateDir)
	if err != nil || len(entries) == 0 {
		return false
	}
	v, s, ok, err := readMarker(filepath.Join(m.templateDir, markerName))
	if err != nil || !ok {
		return false
	}
	return v == version && s == sha256
}

// download fetches url into dst via a temp file and rename. It respects ctx,
// rejects non-200 responses, and caps the body at maxDownloadSize.
func (m *Manager) download(ctx context.Context, url, dst, version string) error {
	if url == "" {
		url = DownloadURL(version)
	}
	if err := os.MkdirAll(m.cacheDir, 0o755); err != nil {
		return fmt.Errorf("payload: create cache dir %s: %w", m.cacheDir, err)
	}
	m.log.Info("downloading runner payload", "version", version, "url", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("payload: build download request: %w", err)
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("payload: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("payload: download %s: unexpected status %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(m.cacheDir, filepath.Base(dst)+".*.tmp")
	if err != nil {
		return fmt.Errorf("payload: create temp file in %s: %w", m.cacheDir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	n, err := io.Copy(tmp, io.LimitReader(contextReader{ctx: ctx, r: resp.Body}, maxDownloadSize+1))
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("payload: read body from %s: %w", url, err)
	}
	if n > maxDownloadSize {
		_ = tmp.Close()
		return fmt.Errorf("payload: download %s exceeds %d bytes", url, maxDownloadSize)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("payload: close temp file %s: %w", tmpName, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("payload: move %s to %s: %w", tmpName, dst, err)
	}
	tmpName = "" // renamed; nothing to clean up
	m.log.Info("runner payload downloaded", "version", version, "path", dst, "bytes", n)
	return nil
}

// verify reports whether the file at path hashes to want. A non-empty want
// is guaranteed by Ensure to be valid hex.
func (m *Manager) verify(ctx context.Context, path, want string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if want == "" { // allowUnverified guaranteed by Ensure
		return true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("payload: open %s for verification: %w", path, err)
	}
	defer f.Close()
	got, err := hashContext(ctx, f)
	if err != nil {
		return false, fmt.Errorf("payload: hash %s: %w", path, err)
	}
	if !strings.EqualFold(got, want) {
		m.log.Warn("payload checksum mismatch", "path", path, "expected", want, "got", got)
		return false, nil
	}
	m.log.Info("payload checksum verified", "path", path, "sha256", got)
	return true, nil
}

func hashContext(ctx context.Context, input io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, contextReader{ctx: ctx, r: input}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extract unpacks the cached tarball into a fresh staging directory
// <templateDir>.tmp, writes the marker file, and atomically promotes the
// staging directory to templateDir.
func (m *Manager) extract(ctx context.Context, cachePath, version, sha256 string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	tmp := m.templateDir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("payload: clear staging dir %s: %w", tmp, err)
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return fmt.Errorf("payload: create staging dir %s: %w", tmp, err)
	}
	defer func() {
		if err != nil {
			if rmErr := os.RemoveAll(tmp); rmErr != nil {
				m.log.Warn("failed to remove staging dir", "path", tmp, "error", rmErr)
			}
		}
	}()

	f, err := os.Open(cachePath)
	if err != nil {
		return fmt.Errorf("payload: open %s: %w", cachePath, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(contextReader{ctx: ctx, r: f})
	if err != nil {
		return fmt.Errorf("payload: gzip %s: %w", cachePath, err)
	}
	defer gz.Close()
	tr := tar.NewReader(contextReader{ctx: ctx, r: gz})
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("payload: read tar from %s: %w", cachePath, err)
		}
		if err := m.extractEntry(ctx, tr, hdr, tmp); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeMarker(filepath.Join(tmp, markerName), version, sha256); err != nil {
		return fmt.Errorf("payload: write template marker: %w", err)
	}
	// fsync the staging directory before promotion so the rename cannot
	// outrun the metadata. This is best-effort because some filesystems
	// do not support directory fsync.
	if d, err := os.Open(tmp); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	m.log.Info("payload extracted", "version", version, "template", tmp)
	if err := m.promote(ctx, tmp); err != nil {
		return err
	}
	m.log.Info("payload template ready", "version", version, "template", m.templateDir)
	return nil
}

// promote preserves the installed tree until the new staging tree is ready.
// If cancellation arrives between the two renames, roll back the old tree.
// After the staging rename commits, remove the backup synchronously.
func (m *Manager) promote(ctx context.Context, staging string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	backup, err := os.MkdirTemp(filepath.Dir(m.templateDir), filepath.Base(m.templateDir)+".previous-*")
	if err != nil {
		return fmt.Errorf("payload: reserve template backup: %w", err)
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	movedOld := false
	defer func() {
		if !movedOld {
			_ = os.RemoveAll(backup)
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(m.templateDir, backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("payload: preserve old template: %w", err)
	} else if err == nil {
		movedOld = true
	}
	rollback := func(cause error) error {
		if movedOld {
			if err := os.Rename(backup, m.templateDir); err != nil {
				return fmt.Errorf("%w; restoring template from %s: %v", cause, backup, err)
			}
			movedOld = false
		}
		return cause
	}
	if err := ctx.Err(); err != nil {
		return rollback(err)
	}
	if err := os.Rename(staging, m.templateDir); err != nil {
		return rollback(fmt.Errorf("payload: promote template: %w", err))
	}
	if movedOld {
		if err := os.RemoveAll(backup); err != nil {
			m.log.Warn("failed to remove previous template", "path", backup, "error", err)
		}
	}
	return nil
}

// evictStaleTarballs removes payload tarballs for versions other than
// version, keeping cacheDir to the current version after upgrades. It is
// best effort: removal failures are logged and never fail Ensure.
func (m *Manager) evictStaleTarballs(version string) {
	want := fmt.Sprintf(cacheFileFmt, version)
	entries, err := os.ReadDir(m.cacheDir)
	if err != nil {
		if !os.IsNotExist(err) {
			m.log.Warn("payload: scan cache dir for stale tarballs", "dir", m.cacheDir, "error", err)
		}
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == want ||
			!strings.HasPrefix(name, payloadPrefix) || !strings.HasSuffix(name, ".tar.gz") {
			continue
		}
		path := filepath.Join(m.cacheDir, name)
		if err := os.Remove(path); err != nil {
			m.log.Warn("payload: evict stale tarball", "path", path, "error", err)
			continue
		}
		m.log.Info("evicted stale payload tarball", "path", path)
	}
}

// extractEntry writes one tar entry under base. Absolute paths and ".."
// traversal are rejected. Symlinks pointing outside base are skipped; hard
// links are skipped; regular files and directories keep their header modes.
func (m *Manager) extractEntry(ctx context.Context, tr *tar.Reader, hdr *tar.Header, base string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name := hdr.Name
	if !safeTarPath(name) {
		return fmt.Errorf("payload: rejecting unsafe tar entry %q", name)
	}
	target := filepath.Join(base, filepath.Clean(name))
	mode := hdr.FileInfo().Mode()
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, 0o700); err != nil {
			return fmt.Errorf("payload: mkdir %s: %w", target, err)
		}
		if err := os.Chmod(target, mode); err != nil {
			return fmt.Errorf("payload: chmod %s: %w", target, err)
		}
	case tar.TypeReg, tar.TypeRegA:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("payload: mkdir %s: %w", filepath.Dir(target), err)
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("payload: create %s: %w", target, err)
		}
		if _, err := io.Copy(out, contextReader{ctx: ctx, r: tr}); err != nil {
			_ = out.Close()
			return fmt.Errorf("payload: write %s: %w", target, err)
		}
		if err := ctx.Err(); err != nil {
			_ = out.Close()
			return err
		}
		// fsync before rename so a crash cannot promote a
		// template with zero-length files.
		if err := out.Sync(); err != nil {
			_ = out.Close()
			return fmt.Errorf("payload: fsync %s: %w", target, err)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("payload: close %s: %w", target, err)
		}
		if err := os.Chmod(target, mode); err != nil {
			return fmt.Errorf("payload: chmod %s: %w", target, err)
		}
	case tar.TypeSymlink:
		if !safeSymlink(base, filepath.Dir(target), hdr.Linkname) {
			m.log.Warn("skipping symlink with unsafe target", "name", name, "linkname", hdr.Linkname)
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("payload: mkdir %s: %w", filepath.Dir(target), err)
		}
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return fmt.Errorf("payload: symlink %s -> %s: %w", target, hdr.Linkname, err)
		}
	case tar.TypeLink:
		m.log.Warn("skipping hard link entry", "name", name, "linkname", hdr.Linkname)
	case tar.TypeXHeader, tar.TypeXGlobalHeader:
		// archive/tar consumes these internally; defensive no-op.
	default:
		return fmt.Errorf("payload: unsupported tar entry type %d for %q", hdr.Typeflag, name)
	}
	return nil
}

// safeTarPath reports whether a tar entry name is safe to extract: not
// absolute and not escaping the extraction root via "..".
func safeTarPath(name string) bool {
	if name == "" || filepath.IsAbs(name) {
		return false
	}
	cleaned := filepath.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// safeSymlink reports whether a symlink with the given linkname, created at
// dir, would stay lexically inside base.
func safeSymlink(base, dir, linkname string) bool {
	if filepath.IsAbs(linkname) {
		return false
	}
	resolved := filepath.Join(dir, linkname)
	rel, err := filepath.Rel(base, resolved)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// checkSHA256 validates the expected checksum: non-empty means it must be a
// 64-char hex digest; empty is only allowed with AllowUnverifiedEnv.
func checkSHA256(want string) error {
	if want == "" {
		if allowUnverified() {
			return nil
		}
		return fmt.Errorf("payload: sha256 checksum required (set %s=1 to allow unverified payloads)", AllowUnverifiedEnv)
	}
	if len(want) != hex.EncodedLen(sha256.Size) {
		return fmt.Errorf("payload: sha256 %q is not a valid hex digest", want)
	}
	if _, err := hex.DecodeString(want); err != nil {
		return fmt.Errorf("payload: sha256 %q is not valid hex: %w", want, err)
	}
	return nil
}

// allowUnverified reports whether AllowUnverifiedEnv is enabled.
func allowUnverified() bool {
	switch strings.ToLower(os.Getenv(AllowUnverifiedEnv)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// safeComponent reports whether name is a single path component suitable for
// building cache file names.
func safeComponent(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return name == filepath.Base(name)
}

// cacheUsable reports whether the cache file exists as a non-empty file.
func cacheUsable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// writeMarker records the version and sha256 of an extracted template,
// fsynced like the payload files it accompanies.
func writeMarker(path, version, sha256 string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(version + "\n" + sha256 + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readMarker parses a marker file. ok is false when the file is absent or
// malformed.
func readMarker(path string) (version, sha256 string, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("payload: read marker %s: %w", path, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		return "", "", false, nil
	}
	return lines[0], lines[1], true, nil
}

// copyTree copies src to dst recursively, preserving modes and recreating
// symlinks as symlinks. WalkDir does not follow symlinks, so nothing can
// escape src.
func copyTree(src, dst string) error { return copyTreeContext(context.Background(), src, dst) }

func copyTreeContext(ctx context.Context, src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case info.IsDir():
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
			return os.Chmod(target, mode)
		case mode.IsRegular():
			return copyFileModeContext(ctx, path, target, mode)
		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
			if err := os.Symlink(link, target); err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", target, link, err)
			}
			return nil
		default:
			return fmt.Errorf("unsupported entry type for %s", path)
		}
	})
}

// copyFileMode copies one regular file and applies mode exactly (immune to
// umask).
func copyFileMode(srcPath, dstPath string, mode os.FileMode) error {
	return copyFileModeContext(context.Background(), srcPath, dstPath, mode)
}

func copyFileModeContext(ctx context.Context, srcPath, dstPath string, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", srcPath, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dstPath, err)
	}
	if _, err := io.Copy(out, contextReader{ctx: ctx, r: in}); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s: %w", srcPath, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dstPath, err)
	}
	if err := os.Chmod(dstPath, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", dstPath, err)
	}
	return nil
}

// contextReader checks cancellation between bounded copy chunks.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	// Restrict each read even if a consumer supplies a much larger buffer.
	if len(p) > 32*1024 {
		p = p[:32*1024]
	}
	n, err := r.r.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}
