//go:build linux

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package overlaylvm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/overlay/overlayutils"
	"github.com/containerd/containerd/snapshots/storage"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/log"
	"github.com/sirupsen/logrus"
)

const (
	upperdirKey = "containerd.io/snapshot/overlay.upperdir"
)

type SnapshotterConfig struct {
	asyncRemove   bool
	upperdirLabel bool
	ms            MetaStore
	mountOptions  []string
	vgName        string
	lvSize        string
	mountParent   string
}

type Opt func(config *SnapshotterConfig) error

// AsynchronousRemove defers removal of filesystem content until
// the Cleanup method is called. Removals will make the snapshot
// referred to by the key unavailable and make the key immediately
// available for re-use.
func AsynchronousRemove(config *SnapshotterConfig) error {
	config.asyncRemove = true
	return nil
}

func WithMountOptions(options []string) Opt {
	return func(config *SnapshotterConfig) error {
		config.mountOptions = append(config.mountOptions, options...)
		return nil
	}
}

// WithUpperdirLabel adds as an optional label
// "containerd.io/snapshot/overlay.upperdir". This stores the location
// of the upperdir that contains the changeset between the labelled
// snapshot and its parent.
func WithUpperdirLabel(config *SnapshotterConfig) error {
	config.upperdirLabel = true
	return nil
}

func WithVolumeGroupName(vgName string) Opt {
	return func(config *SnapshotterConfig) error {
		config.vgName = vgName
		return nil
	}
}

func WithLogicalVolumeSize(lvSize string) Opt {
	return func(config *SnapshotterConfig) error {
		config.lvSize = lvSize
		return nil
	}
}

func WithMountParent(mountParent string) Opt {
	return func(config *SnapshotterConfig) error {
		config.mountParent = mountParent
		return nil
	}
}

// ...existing overlay options...

type MetaStore interface {
	TransactionContext(ctx context.Context, writable bool) (context.Context, storage.Transactor, error)
	WithTransaction(ctx context.Context, writable bool, fn storage.TransactionCallback) error
	Close() error
}

func WithMetaStore(ms MetaStore) Opt {
	return func(config *SnapshotterConfig) error {
		config.ms = ms
		return nil
	}
}

type snapshotter struct {
	root          string
	ms            MetaStore
	asyncRemove   bool
	upperdirLabel bool
	options       []string
	vgName        string
	lvSize        string
	mountParent   string
}

func NewSnapshotter(root string, opts ...Opt) (snapshots.Snapshotter, error) {
	var config SnapshotterConfig
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	supportsDType, err := fs.SupportsDType(root)
	if err != nil {
		return nil, err
	}
	if !supportsDType {
		return nil, fmt.Errorf("%s does not support d_type. If the backing filesystem is xfs, please reformat with ftype=1 to enable d_type support", root)
	}
	if config.ms == nil {
		config.ms, err = storage.NewMetaStore(filepath.Join(root, "metadata.db"))
		if err != nil {
			return nil, err
		}
	}

	if err := os.Mkdir(filepath.Join(root, "snapshots"), 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}

	if !hasOption(config.mountOptions, "userxattr", false) {
		// figure out whether "userxattr" option is recognized by the kernel && needed
		userxattr, err := overlayutils.NeedsUserXAttr(root)
		if err != nil {
			logrus.WithError(err).Warnf("cannot detect whether \"userxattr\" option needs to be used, assuming to be %v", userxattr)
		}
		if userxattr {
			config.mountOptions = append(config.mountOptions, "userxattr")
		}
	}

	if !hasOption(config.mountOptions, "index", false) && supportsIndex() {
		config.mountOptions = append(config.mountOptions, "index=off")
	}

	return &snapshotter{
		root:          root,
		ms:            config.ms,
		asyncRemove:   config.asyncRemove,
		upperdirLabel: config.upperdirLabel,
		options:       config.mountOptions,
		vgName:        config.vgName,
		lvSize:        config.lvSize,
		mountParent:   config.mountParent,
	}, nil
}

func hasOption(options []string, key string, hasValue bool) bool {
	for _, option := range options {
		if hasValue {
			if strings.HasPrefix(option, key) && len(option) > len(key) && option[len(key)] == '=' {
				return true
			}
		} else if option == key {
			return true
		}
	}
	return false
}

// Stat returns the info for an active or committed snapshot by name or
// key.
//
// Should be used for parent resolution, existence checks and to discern
// the kind of snapshot.
func (o *snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	var id string
	var info snapshots.Info
	if err := o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		// var usage snapshots.Usage
		var err error
		// id, info, usage, err = storage.GetInfo(ctx, key)
		id, info, _, err = storage.GetInfo(ctx, key)
		return err
	}); err != nil {
		return info, err
	}
	if o.upperdirLabel {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		info.Labels[upperdirKey] = o.upperPath(id)
	}
	return info, nil
}

// Update updates the info for a snapshot.
//
// Only mutable properties of a snapshot may be updated.
func (o *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	var newInfo snapshots.Info
	err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		var err error
		newInfo, err = storage.UpdateInfo(ctx, info, fieldpaths...)
		if err != nil {
			return err
		}
		if o.upperdirLabel {
			id, _, _, err := storage.GetInfo(ctx, newInfo.Name)
			if err != nil {
				return err
			}
			if newInfo.Labels == nil {
				newInfo.Labels = make(map[string]string)
			}
			newInfo.Labels[upperdirKey] = o.upperPath(id)
		}
		return nil
	})
	return newInfo, err
}

// Usage returns the resource usage of an active or committed snapshot
// excluding the usage of parent snapshots.
//
// The running time of this call for active snapshots is dependent on
// implementation, but may be proportional to the size of the resource.
// Callers should take this into consideration. Implementations should
// attempt to honor context cancellation and avoid taking locks when making
// the calculation.
func (o *snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	var (
		usage snapshots.Usage
		info  snapshots.Info
		id    string
	)
	if err := o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		var err error
		id, info, usage, err = storage.GetInfo(ctx, key)
		return err
	}); err != nil {
		return usage, err
	}

	if info.Kind == snapshots.KindActive {
		upperPath := o.upperPath(id)
		du, err := fs.DiskUsage(ctx, upperPath)
		if err != nil {
			return snapshots.Usage{}, err
		}
		usage = snapshots.Usage(du)
	}
	return usage, nil
}

// View behaves identically to Prepare except the result may not be
// committed back to the snapshot snapshotter. View returns a readonly view on
// the parent, with the active snapshot being tracked by the given key.
//
// This method operates identically to Prepare, except the mounts returned
// may have the readonly flag set. Any modifications to the underlying
// filesystem will be ignored. Implementations may perform this in a more
// efficient manner that differs from what would be attempted with
// `Prepare`.
//
// Commit may not be called on the provided key and will return an error.
// To collect the resources associated with key, Remove must be called with
// key as the argument.
func (o *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return o.createSnapshot(ctx, snapshots.KindView, key, parent, opts...)
}

// Mounts returns the mounts for the active snapshot transaction identified
// by key. Can be called on a read-write or readonly transaction. This is
// available only for active snapshots.
//
// This can be used to recover mounts after calling View or Prepare.
func (o *snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	var s storage.Snapshot
	if err := o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		var err error
		s, err = storage.GetSnapshot(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get active mount: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return o.mounts(s), nil
}

// Commit captures the changes between key and its parent into a snapshot
// identified by name.  The name can then be used with the snapshotter's other
// methods to create subsequent snapshots.
//
// A committed snapshot will be created under name with the parent of the
// active snapshot.
//
// After commit, the snapshot identified by key is removed.
func (o *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		id, _, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}
		usage, err := fs.DiskUsage(ctx, o.upperPath(id))
		if err != nil {
			return err
		}
		if _, err = storage.CommitActive(ctx, key, name, snapshots.Usage(usage), opts...); err != nil {
			return fmt.Errorf("failed to commit snapshot %s: %w", key, err)
		}
		return nil
	})
}

// Remove the committed or active snapshot by the provided key.
//
// All resources associated with the key will be removed.
//
// If the snapshot is a parent of another snapshot, its children must be
// removed before proceeding.
func (o *snapshotter) Remove(ctx context.Context, key string) error {
	var removals []string
	defer func() {
		for _, dir := range removals {
			if err := os.RemoveAll(dir); err != nil {
				log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
			}
		}
	}()
	return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		_, _, err := storage.Remove(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to remove snapshot %s: %w", key, err)
		}
		if !o.asyncRemove {
			removals, err = o.getCleanupDirectories(ctx)
			if err != nil {
				return fmt.Errorf("unable to get directories for removal: %w", err)
			}
		}
		return nil
	})
}

// Walk will call the provided function for each snapshot in the
// snapshotter which match the provided filters. If no filters are
// given all items will be walked.
// Filters:
//
//	name
//	parent
//	kind (active,view,committed)
//	labels.(label)
func (o *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	return o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		if o.upperdirLabel {
			return storage.WalkInfo(ctx, func(ctx context.Context, info snapshots.Info) error {
				id, _, _, err := storage.GetInfo(ctx, info.Name)
				if err != nil {
					return err
				}
				if info.Labels == nil {
					info.Labels = make(map[string]string)
				}
				info.Labels[upperdirKey] = o.upperPath(id)
				return fn(ctx, info)
			}, filters...)
		}
		return storage.WalkInfo(ctx, fn, filters...)
	})
}

// Cleaner defines a type capable of performing asynchronous resource cleanup.
// The Cleaner interface should be used by snapshotters which implement fast
// removal and deferred resource cleanup. This prevents snapshots from needing
// to perform lengthy resource cleanup before acknowledging a snapshot key
// has been removed and available for re-use. This is also useful when
// performing multi-key removal with the intent of cleaning up all the
// resources after each snapshot key has been removed.
func (o *snapshotter) Cleanup(ctx context.Context) error {
	cleanup, err := o.cleanupDirectories(ctx)
	if err != nil {
		return err
	}
	for _, dir := range cleanup {
		if err := os.RemoveAll(dir); err != nil {
			log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
		}
	}
	return nil
}

// Prepare creates an active snapshot identified by key descending from the
// provided parent.  The returned mounts can be used to mount the snapshot
// to capture changes.
//
// If a parent is provided, after performing the mounts, the destination
// will start with the content of the parent. The parent must be a
// committed snapshot. Changes to the mounted destination will be captured
// in relation to the parent. The default parent, "", is an empty
// directory.
//
// The changes may be saved to a committed snapshot by calling Commit. When
// one is done with the transaction, Remove should be called on the key.
//
// Multiple calls to Prepare or View with the same key should fail.
func (o *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return o.createSnapshot(ctx, snapshots.KindActive, key, parent, opts...)
}

func (o *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts ...snapshots.Opt) (_ []mount.Mount, err error) {
	var (
		s        storage.Snapshot
		td, path string
	)

	defer func() {
		if err != nil {
			if td != "" {
				if err1 := os.RemoveAll(td); err1 != nil {
					log.G(ctx).WithError(err1).Warn("failed to cleanup temp snapshot directory")
				}
			}
			if path != "" {
				if err1 := os.RemoveAll(path); err1 != nil {
					log.G(ctx).WithError(err1).WithField("path", path).Error("failed to reclaim snapshot directory, directory may need removal")
					err = fmt.Errorf("failed to remove path: %v: %w", err1, err)
				}
			}
		}
	}()

	if err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) (err error) {
		snapshotDir := filepath.Join(o.root, "snapshots")
		td, err = o.prepareDirectory(ctx, snapshotDir, kind)
		if err != nil {
			return fmt.Errorf("failed to create prepare snapshot dir: %w", err)
		}

		s, err = storage.CreateSnapshot(ctx, kind, key, parent, opts...)
		if err != nil {
			return fmt.Errorf("failed to create snapshot: %w", err)
		}

		if len(s.ParentIDs) > 0 {
			st, err := os.Stat(o.upperPath(s.ParentIDs[0]))
			if err != nil {
				return fmt.Errorf("failed to stat parent: %w", err)
			}

			stat := st.Sys().(*syscall.Stat_t)
			if err := os.Lchown(filepath.Join(td, "fs"), int(stat.Uid), int(stat.Gid)); err != nil {
				return fmt.Errorf("failed to chown: %w", err)
			}
		}

		path = filepath.Join(snapshotDir, s.ID)
		if err = os.Rename(td, path); err != nil {
			return fmt.Errorf("failed to rename: %w", err)
		}
		td = ""

		// LVM logic for active snapshot
		if kind == snapshots.KindActive {
			var base snapshots.Info
			for _, opt := range opts {
				if err := opt(&base); err != nil {
					return fmt.Errorf("failed to apply option: %w", err)
				}
			}
			upperDir := filepath.Join(path, "fs", "upper")
			workDir := filepath.Join(path, "fs", "work")
			if err := o.createLVMVolume(s.ID, upperDir); err != nil {
				return fmt.Errorf("failed to create LVM volume: %w", err)
			}
			if err := os.MkdirAll(workDir, 0755); err != nil {
				return fmt.Errorf("failed to create workdir: %w", err)
			}
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return o.mounts(s), nil
}

func (o *snapshotter) createLVMVolume(id, upperDir string) error {
	lvName := "snap_" + strings.ReplaceAll(id, "-", "_")
	lvPath := fmt.Sprintf("/dev/%s/%s", o.vgName, lvName)
	mountPoint := filepath.Join(o.mountParent, lvName)

	// Create LV
	cmd := exec.Command("lvcreate", "-n", lvName, "-L", o.lvSize, o.vgName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("lvcreate failed: %s: %w", string(out), err)
	}
	// Make filesystem
	cmd = exec.Command("mkfs.ext4", lvPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %s: %w", string(out), err)
	}
	// Mount LV
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return err
	}
	cmd = exec.Command("mount", lvPath, mountPoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount failed: %s: %w", string(out), err)
	}
	// Create upperdir inside mount
	upperDirInLV := filepath.Join(mountPoint, "upper")
	if err := os.MkdirAll(upperDirInLV, 0755); err != nil {
		return err
	}
	// Symlink upperdir to expected location
	if err := os.Symlink(upperDirInLV, upperDir); err != nil {
		return err
	}
	return nil
}

// ...existing code for prepareDirectory, mounts, upperPath, workPath, Close, supportsIndex...

func (o *snapshotter) Close() error {
	if o.ms != nil {
		return o.ms.Close()
	}
	return nil
}

// Add missing Snapshotter interface methods

func (o *snapshotter) cleanupDirectories(ctx context.Context) ([]string, error) {
	var cleanupDirs []string
	if err := o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		var err error
		cleanupDirs, err = o.getCleanupDirectories(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	return cleanupDirs, nil
}

func (o *snapshotter) getCleanupDirectories(ctx context.Context) ([]string, error) {
	ids, err := storage.IDMap(ctx)
	if err != nil {
		return nil, err
	}
	snapshotDir := filepath.Join(o.root, "snapshots")
	fd, err := os.Open(snapshotDir)
	if err != nil {
		return nil, err
	}
	defer fd.Close()
	dirs, err := fd.Readdirnames(0)
	if err != nil {
		return nil, err
	}
	cleanup := []string{}
	for _, d := range dirs {
		if _, ok := ids[d]; ok {
			continue
		}
		cleanup = append(cleanup, filepath.Join(snapshotDir, d))
	}
	return cleanup, nil
}

func (o *snapshotter) upperPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "fs")
}

func (o *snapshotter) workPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "work")
}

func (o *snapshotter) prepareDirectory(ctx context.Context, snapshotDir string, kind snapshots.Kind) (string, error) {
	td, err := os.MkdirTemp(snapshotDir, "new-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	if err := os.Mkdir(filepath.Join(td, "fs"), 0755); err != nil {
		return td, err
	}

	if kind == snapshots.KindActive {
		if err := os.Mkdir(filepath.Join(td, "work"), 0711); err != nil {
			return td, err
		}
	}

	return td, nil
}

func (o *snapshotter) mounts(s storage.Snapshot) []mount.Mount {
	if len(s.ParentIDs) == 0 {
		// if we only have one layer/no parents then just return a bind mount as overlay
		// will not work
		roFlag := "rw"
		if s.Kind == snapshots.KindView {
			roFlag = "ro"
		}

		return []mount.Mount{
			{
				Source: o.upperPath(s.ID),
				Type:   "bind",
				Options: []string{
					roFlag,
					"rbind",
				},
			},
		}
	}

	options := o.options
	if s.Kind == snapshots.KindActive {
		options = append(options,
			fmt.Sprintf("workdir=%s", o.workPath(s.ID)),
			fmt.Sprintf("upperdir=%s", o.upperPath(s.ID)),
		)
	} else if len(s.ParentIDs) == 1 {
		return []mount.Mount{
			{
				Source: o.upperPath(s.ParentIDs[0]),
				Type:   "bind",
				Options: []string{
					"ro",
					"rbind",
				},
			},
		}
	}

	parentPaths := make([]string, len(s.ParentIDs))
	for i := range s.ParentIDs {
		parentPaths[i] = o.upperPath(s.ParentIDs[i])
	}

	options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(parentPaths, ":")))
	return []mount.Mount{
		{
			Type:    "overlay",
			Source:  "overlay",
			Options: options,
		},
	}

}

// supportsIndex checks whether the "index=off" option is supported by the kernel.
func supportsIndex() bool {
	if _, err := os.Stat("/sys/module/overlay/parameters/index"); err == nil {
		return true
	}
	return false
}
