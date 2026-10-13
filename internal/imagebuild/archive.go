package imagebuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// isTarMeta reports whether a header is tar bookkeeping that must never be
// treated as a real entry. Go's archive/tar SURFACES a leading
// `TypeXGlobalHeader` (Forgejo archives begin with a `pax_global_header`), and
// a `TypeXHeader` may appear too; both would otherwise be mistaken for the
// archive's top-level directory name.
func isTarMeta(hdr *tar.Header) bool {
	return hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeXHeader
}

// archiveTop decides which single leading path segment to strip. Forgejo wraps
// every archive in one `<repo>/` directory, but a tar stream may begin with PAX
// metadata and an archive may (in principle) have no wrapper at all. Strip the
// leading segment ONLY when every real entry shares the same first segment AND
// that entry is actually nested (its name contains a separator) — otherwise
// nothing is stripped, so a root-level file is never dropped.
func archiveTop(tarball []byte) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return "", fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seg := ""
	nested := false
	uniform := true
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar: %w", err)
		}
		if isTarMeta(hdr) {
			continue
		}
		name := filepath.ToSlash(hdr.Name)
		if strings.Contains(name, "/") {
			nested = true
		}
		first := strings.SplitN(strings.Trim(name, "/"), "/", 2)[0]
		if first == "" {
			continue
		}
		if seg == "" {
			seg = first
		} else if first != seg {
			uniform = false
		}
	}
	if nested && uniform && seg != "" {
		return seg, nil
	}
	return "", nil
}

// Extract unpacks a tar.gz repository archive into a fresh temp directory,
// stripping the archive's single top-level directory (Forgejo wraps the tree).
// The returned cleanup removes the directory.
func Extract(tarball []byte, maxBytes int64) (string, func(), error) {
	if maxBytes <= 0 {
		maxBytes = 512 << 20
	}
	top, err := archiveTop(tarball)
	if err != nil {
		return "", nil, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return "", nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	dir, err := os.MkdirTemp("", "sandbox-build-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("tar: %w", err)
		}
		if isTarMeta(hdr) {
			continue
		}
		name := filepath.ToSlash(hdr.Name)
		rel := name
		if top != "" {
			rel = strings.TrimPrefix(name, top)
		}
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			continue
		}
		// Reject path escapes.
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if !strings.HasPrefix(target, dir+string(os.PathSeparator)) && target != dir {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				cleanup()
				return "", nil, err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxBytes {
				cleanup()
				return "", nil, fmt.Errorf("build context exceeds %d bytes", maxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				cleanup()
				return "", nil, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				cleanup()
				return "", nil, err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				cleanup()
				return "", nil, err
			}
			_ = f.Close()
		}
	}
	return dir, cleanup, nil
}

// tail returns the last n bytes of s (the useful end of a build log).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…\n" + s[len(s)-n:]
}
