package imagebuild

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func writeTree(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "dist", "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "app"), []byte("binary-content"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "dist", "index.html"), []byte("<html></html>"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "dist", "assets", "app.js"), []byte("console.log(1)"), 0o644)
}

// TestTarDirPacksTree: the exported tree becomes a tar with the same RELATIVE
// paths, so a caller can untar it straight into a directory.
func TestTarDirPacksTree(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root)

	data, count, total, truncated, err := tarDir(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("small tree must not be truncated")
	}
	if count != 3 {
		t.Errorf("count = %d, want 3 files", count)
	}
	if total <= 0 {
		t.Errorf("total = %d, want > 0", total)
	}

	names := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			names[hdr.Name] = string(b)
		}
	}
	if names["app"] != "binary-content" {
		t.Errorf("app = %q", names["app"])
	}
	if names["dist/index.html"] != "<html></html>" {
		t.Errorf("dist/index.html = %q", names["dist/index.html"])
	}
	if names["dist/assets/app.js"] != "console.log(1)" {
		t.Errorf("nested file lost: %q", names["dist/assets/app.js"])
	}
}

// TestTarDirRefusesOversizeTree: exceeding the cap must FAIL the export, not
// return a partial tree — a half-copied artifact is worse than a clear error.
func TestTarDirRefusesOversizeTree(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root)

	data, count, _, truncated, err := tarDir(root, 5) // cap far below the tree size
	if err != nil {
		t.Fatalf("an oversize tree reports truncation, not an error: %v", err)
	}
	if !truncated {
		t.Error("must report truncation")
	}
	if data != nil || count != 0 {
		t.Errorf("must not hand back a partial tree: %d bytes, %d files", len(data), count)
	}
}

// TestTarDirEmpty: an empty export is valid (the stage produced nothing), and
// reports zero files rather than an error.
func TestTarDirEmpty(t *testing.T) {
	root := t.TempDir()
	data, count, total, truncated, err := tarDir(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 || total != 0 || truncated {
		t.Errorf("empty tree: count=%d total=%d truncated=%v", count, total, truncated)
	}
	_ = data
}
