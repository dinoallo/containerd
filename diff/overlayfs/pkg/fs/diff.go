package fs

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/log"
	fs "github.com/containerd/continuity/fs"
)

// ChangeFunc is the type of function called for each change
// computed during a directory changes calculation.
type ChangeFunc func(fs.ChangeKind, string, os.FileInfo, error) error

type diffDirOptions struct {
	deleteChange func(string, string, os.FileInfo, ChangeFunc) (bool, error)
}

// Gnu tar and the go tar writer don't have sub-second mtime
// precision, which is problematic when we apply changes via tar
// files, we handle this by comparing for exact times, *or* same
// second count and either a or b having exactly 0 nanoseconds
func sameFsTime(a, b time.Time) bool {
	return a.Equal(b) ||
		(a.Unix() == b.Unix() &&
			(a.Nanosecond() == 0 || b.Nanosecond() == 0))
}

func DiffDirChanges(baseDir string, diffLayers []string, changeFns []ChangeFunc) error {
	o := &diffDirOptions{
		deleteChange: overlayFSWhiteoutConvert,
	}
	mergeFileView, err := getMergeFileView(diffLayers)
	if err != nil {
		return fmt.Errorf("failed to get merge file view: %w", err)
	}

	changedDirs := make(map[string]struct{})
	return mergeFileView.Files.Walk(func(key, fullPath string, value any) error {
		node, ok := value.(FileNode)
		if !ok {
			return nil
		}
		layerIndex := node.LayerIndex
		diffDir := diffLayers[layerIndex]
		f, err := os.Lstat(fullPath)
		if err != nil {
			return err
		}
		// Rebase path
		path, err := filepath.Rel(diffDir, fullPath)
		if err != nil {
			return err
		}

		path = filepath.Join(string(os.PathSeparator), path)

		// Skip root
		if path == string(os.PathSeparator) {
			return nil
		}

		var kind fs.ChangeKind

		deletedFile := false
		changeFn := changeFns[layerIndex]

		if o.deleteChange != nil {
			deletedFile, err = o.deleteChange(diffDir, path, f, changeFn)
			if err != nil {
				return err
			}

			_, err = os.Stat(filepath.Join(baseDir, path))
			if err != nil {
				if !os.IsNotExist(err) {
					return err
				}
				deletedFile = false
			}
		}

		// Find out what kind of modification happened
		if deletedFile {
			kind = fs.ChangeKindDelete
		} else {
			// Otherwise, the file was added
			kind = fs.ChangeKindAdd

			// ...Unless it already existed in a baseDir, in which case, it's a modification
			stat, err := os.Stat(filepath.Join(baseDir, path))
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			if err == nil {
				// The file existed in the baseDir, so that's a modification

				// However, if it's a directory, maybe it wasn't actually modified.
				// If you modify /foo/bar/baz, then /foo will be part of the changed files only because it's the parent of bar
				if stat.IsDir() && f.IsDir() {
					if f.Size() == stat.Size() && f.Mode() == stat.Mode() && sameFsTime(f.ModTime(), stat.ModTime()) {
						// Both directories are the same, don't record the change
						return nil
					}
				}
				kind = fs.ChangeKindModify
			}
		}

		// If /foo/bar/file.txt is modified, then /foo/bar must be part of the changed files.
		// This block is here to ensure the change is recorded even if the
		// modify time, mode and size of the parent directory in the rw and ro layers are all equal.
		// Check https://github.com/docker/docker/pull/13590 for details.
		if f.IsDir() {
			changedDirs[path] = struct{}{}
		}

		if kind == fs.ChangeKindAdd || kind == fs.ChangeKindDelete {
			parent := filepath.Dir(path)

			if _, ok := changedDirs[parent]; !ok && parent != "/" {
				pi, err := os.Stat(filepath.Join(diffDir, parent))
				if err := changeFn(fs.ChangeKindModify, parent, pi, err); err != nil {
					return err
				}
				changedDirs[parent] = struct{}{}
			}
		}

		if kind == fs.ChangeKindDelete {
			f = nil
		}
		return changeFn(kind, path, f, nil)
	})
}

func getMergeFileView(diffLayers []string) (*MergedFileView, error) {
	view := NewMergedFileView()
	for layerIndex := len(diffLayers) - 1; layerIndex >= 0; layerIndex-- {
		layerDir := diffLayers[layerIndex]
		// Walk all files in layerDir using WalkDir
		err := filepath.WalkDir(layerDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// Call MergeFileIfNecessary for each file
			if ifMerged, mergeErr := view.MergeFileIfNecessary(path, layerIndex); mergeErr != nil {
				return mergeErr
			} else {
				log.L.Debugf("Merging file: %s, layerIndex: %d, merged: %v, err: %v", path, layerIndex, ifMerged, mergeErr)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("error walking layer %s: %w", layerDir, err)
		}
	}
	return view, nil
}
