//go:build linux
// +build linux

package fs

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"golang.org/x/sys/unix"
)

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root to mount overlayfs and create whiteouts (char device 0:0)")
	}
}

func createFile(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func createWhiteoutChar(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// mknod char device 0:0
	if err := unix.Mknod(path, unix.S_IFCHR|0o600, int(unix.Mkdev(0, 0))); err != nil {
		t.Fatalf("mknod whiteout: %v", err)
	}
}

func walkFilesRelative(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func mountOverlay(t *testing.T, lower, upper, work, merged string) func() {
	t.Helper()
	data := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if err := unix.Mount("overlay", merged, "overlay", 0, data); err != nil {
		// Common cases on non-overlay systems or restricted envs
		if err == unix.ENODEV || err == unix.EPERM || err == unix.EINVAL { //nolint:errorlint
			t.Skipf("overlayfs not available: mount error: %v", err)
		}
		t.Fatalf("mount overlay: %v", err)
	}
	return func() {
		_ = unix.Unmount(merged, 0)
	}
}

func TestOverlayFSTrieMergeMatchesOverlayMount(t *testing.T) {
	requireRoot(t)

	base := t.TempDir()
	lower := filepath.Join(base, "lower")
	upper := filepath.Join(base, "upper")
	work := filepath.Join(base, "work")
	merged := filepath.Join(base, "merged")
	for _, d := range []string{lower, upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Prepare lower contents
	createFile(t, filepath.Join(lower, "keep.txt"), "lower-keep")
	createFile(t, filepath.Join(lower, "rm.txt"), "to-be-removed")
	createFile(t, filepath.Join(lower, "dir1/file1.txt"), "lower-file1")

	// Prepare upper: override keep.txt, add new.txt, whiteout rm.txt, override dir1/file1.txt
	createFile(t, filepath.Join(upper, "keep.txt"), "upper-keep")
	createFile(t, filepath.Join(upper, "new.txt"), "upper-new")
	createFile(t, filepath.Join(upper, "dir1/file1.txt"), "upper-file1")
	createWhiteoutChar(t, filepath.Join(upper, "rm.txt"))

	// Mount overlay
	unmount := mountOverlay(t, lower, upper, work, merged)
	defer unmount()

	// Expected files from merged view
	expected, err := walkFilesRelative(merged)
	if err != nil {
		t.Fatalf("walk merged: %v", err)
	}

	// Build trie by merging from topmost to lowest
	layers := []string{lower, upper}
	trie := NewOverlayFSTrie(layers)

	// Track whiteouts encountered in upper layers by logical relative path
	suppressed := map[string]bool{}

	// Iterate layers: upper (index 1) then lower (index 0)
	oldWD, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWD) }()

	for idx := len(layers) - 1; idx >= 0; idx-- {
		root := layers[idx]
		if err := os.Chdir(root); err != nil {
			t.Fatalf("chdir %s: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			// Update trie state first
			if _, mErr := trie.Merge(FileMeta{FullPath: rel, LayerIndex: idx}); mErr != nil {
				return mErr
			}
			// Determine if this path is a whiteout in this layer; if so, suppress from lowers
			if w, wErr := isWhiteout(rel); wErr == nil && w {
				suppressed[rel] = true
				return nil
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk layer %s: %v", root, err)
		}
	}

	// Now, collect the merged logical file set produced by our process:
	// For each layer from top to bottom, include non-removed files not suppressed by an upper whiteout.
	gotSet := map[string]struct{}{}
	for idx := len(layers) - 1; idx >= 0; idx-- {
		root := layers[idx]
		if err := os.Chdir(root); err != nil {
			t.Fatalf("chdir %s: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if suppressed[rel] {
				return nil
			}
			// Skip if this is a whiteout in this layer
			if w, wErr := isWhiteout(rel); wErr == nil && w {
				return nil
			}
			gotSet[rel] = struct{}{}
			return nil
		})
		if err != nil {
			t.Fatalf("walk layer %s for collect: %v", root, err)
		}
	}

	var got []string
	for k := range gotSet {
		got = append(got, k)
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("merged file set mismatch:\n got: %v\nwant: %v", got, expected)
	}
}
