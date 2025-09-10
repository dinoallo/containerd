package fs

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/diff/overlayfs/pkg/trie"
)

type FileNode struct {
	LayerIndex int
}

type MergedFileView struct {
	Files *trie.PathTrie
}

func NewMergedFileView() *MergedFileView {
	return &MergedFileView{
		Files: trie.NewPathTrie(""),
	}
}

// MergeFileIfNecessary checks if the file at filePath is relevant (not deleted or shadowed by a whiteout)
// in the context of the current layerIndex. If it is relevant, it adds it to the trie.
// It returns true if the file is relevant and added, false if it is not relevant (deleted or shadowed).
// TODO: make this layerIndex aware
func (v *MergedFileView) MergeFileIfNecessary(filePath string, layerIndex int) (bool, error) {
	var (
		fileIsRelevant bool = true
	)
	walkFunc := func(part, path string, _fileNode any) error {
		if !fileIsRelevant {
			// The file is already marked as relevant, no need to continue walking
			return nil
		}
		if path == filePath {
			// path is the file itself
			if _fileNode == nil {
				return nil
			}
			fileNode, ok := _fileNode.(FileNode)
			if !ok {
				return nil
			}
			if fileNode.LayerIndex > layerIndex {
				// A more recent layer has a more recent version of this file, so this file is not relevant
				fileIsRelevant = false
			}
			return nil
		}
		// path is a parent directory of filePath
		// Check if there is an opaque whiteout in any parent directory
		if v.CheckOpaqueDir(path) {
			// This file is whited out by an opaque dir, so it is not relevant
			fileIsRelevant = false
			return nil
		}
		// Check if the file is whited out in the parent directory
		if path == filepath.Dir(filePath) && v.CheckWhiteout(path, filePath) {
			// This file is whited out, so it is not relevant
			fileIsRelevant = false
		}
		return nil
	}

	if err := v.Files.WalkPath(filePath, walkFunc); err != nil {
		return false, err
	} else if !fileIsRelevant {
		return false, nil
	}
	// This file is relevant, add it to the trie
	v.Files.Put(filePath, FileNode{
		LayerIndex: layerIndex,
	})
	return fileIsRelevant, nil
}

// CheckWhiteout checks if the file at path is shadowed by a whiteout file in the context of ppath.
// It returns true if the file is shadowed by a whiteout, false otherwise.
func (v *MergedFileView) CheckWhiteout(ppath, path string) bool {
	// Additional check: if ppath is the parent directory of path, check for whiteout file
	whiteOutFile := filepath.Join(ppath, ".wh."+filepath.Base(path))
	return v.Files.Get(whiteOutFile) != nil
}

func (v *MergedFileView) CheckOpaqueDir(ppath string) bool {
	// Check if under ppath's directory, there is a opaque whiteout entry
	whiteoutOpaqueDir := filepath.Join(ppath, ".wh..wh..opq")
	return v.Files.Get(whiteoutOpaqueDir) != nil
}

func (v *MergedFileView) DebugPrint() {
	walkFunc := func(key, fullPath string, value any) error {
		fmt.Printf("Key: %s, FullPath: %s\n", key, fullPath)
		fileNode, ok := value.(FileNode)
		if !ok {
			return nil
		}
		fmt.Printf("LayerIndex: %d\n", fileNode.LayerIndex)
		return nil
	}
	v.Files.Walk(walkFunc)
}

func (v *MergedFileView) DebugStringPrint() string {
	var result strings.Builder
	walkFunc := func(key, fullPath string, value any) error {
		result.WriteString(fmt.Sprintf("Key: %s, FullPath: %s\n", key, fullPath))
		fileNode, ok := value.(FileNode)
		if !ok {
			return nil
		}
		result.WriteString(fmt.Sprintf("LayerIndex: %d\n", fileNode.LayerIndex))
		return nil
	}
	v.Files.Walk(walkFunc)
	return result.String()
}
