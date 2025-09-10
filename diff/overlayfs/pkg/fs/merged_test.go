package fs

import (
	"path/filepath"
	"testing"
)

func TestCheckWhiteout(t *testing.T) {
	view := NewMergedFileView()
	// Simulate a whiteout file
	whiteoutFile := filepath.Join("/foo/bar1", ".wh.baz")
	view.Files.Put(whiteoutFile, FileNode{LayerIndex: 2})
	if !view.CheckWhiteout("/foo/bar1", "/foo/bar1/baz") {
		t.Errorf("Expected whiteout for file")
	}

	// No whiteout
	if view.CheckWhiteout("/foo", "/foo/qux") {
		t.Errorf("Did not expect whiteout for non-whiteouted file")
	}
}

func TestCheckOpaqueDir(t *testing.T) {
	view := NewMergedFileView()
	// Simulate a whiteout opaque dir
	whiteoutOpaqueDir := filepath.Join("/foo/bar1", ".wh..wh..opq")
	view.Files.Put(whiteoutOpaqueDir, FileNode{LayerIndex: 1})
	if !view.CheckOpaqueDir("/foo/bar1") {
		t.Errorf("Expected opaque dir to be detected")
	}
	// Negative test
	if view.CheckOpaqueDir("/foo/bar2") {
		t.Errorf("Did not expect opaque dir for unrelated directory")
	}
}

func TestMergeFileIfNecessary(t *testing.T) {
	view := NewMergedFileView()
	// Add a file in layer 1
	if ok, err := view.MergeFileIfNecessary("/foo/bar", 1); err != nil {
		t.Errorf("MergeFileIfNecessary failed: %v", err)
	} else if !ok {
		t.Errorf("Expected file to be relevant and added, got err=%v ok=%v", err, ok)
	}
	// Add the same file in a later layer (should shadow previous)
	if ok, err := view.MergeFileIfNecessary("/foo/bar", 2); err != nil {
		t.Errorf("MergeFileIfNecessary failed: %v", err)
	} else if !ok {
		t.Errorf("Expected file to be relevant and added in later layer, got err=%v ok=%v", err, ok)
	}
	// Add a whiteout for the file in layer 2
	whiteoutFile := filepath.Join("/foo", ".wh.baz")
	view.Files.Put(whiteoutFile, FileNode{LayerIndex: 2})
	if ok, err := view.MergeFileIfNecessary("/foo/baz", 2); err != nil {
		t.Errorf("Unexpected error: %v", err)
	} else if ok {
		t.Errorf("Expected file to be shadowed by whiteout and not added")
	}

	// Whiteout in nested directory
	nestedWhiteoutOpaqueDir := filepath.Join("/foo/bar2/baz", ".wh..wh..opq")
	view.Files.Put(nestedWhiteoutOpaqueDir, FileNode{LayerIndex: 3})
	if ok, err := view.MergeFileIfNecessary("/foo/bar2/baz/qux/longfilename.txt", 3); err != nil {
		t.Errorf("MergeFileIfNecessary failed: %v", err)
	} else if ok {
		t.Errorf("Expected whiteout for nested opaque dir")
	}
	// This file is also whited out by the opaque dir above
	if ok, err := view.MergeFileIfNecessary("/foo/bar2/baz/anotherfile.txt", 3); err != nil {
		t.Errorf("MergeFileIfNecessary failed: %v", err)
	} else if ok {
		t.Errorf("Expected whiteout for another file in the opaque dir")
	}
	if ok, err := view.MergeFileIfNecessary("/foo/bar3", 4); err != nil {
		t.Errorf("MergeFileIfNecessary failed: %v", err)
	} else if !ok {
		t.Errorf("Expected file to be relevant and added in later layer")
	}
	t.Logf("Final merged files: %+v", view.DebugStringPrint())
}
