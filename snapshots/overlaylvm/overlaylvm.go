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
	"github.com/containerd/containerd/snapshots/storage"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/log"
)

const upperdirKey = "containerd.io/snapshot/overlay.upperdir"

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

func WithVGName(vgName string) Opt {
	return func(config *SnapshotterConfig) error {
		config.vgName = vgName
		return nil
	}
}

func WithLVSize(lvSize string) Opt {
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

// ...existing code for hasOption, Stat, Update, Usage, View, Mounts, Commit, Remove, Walk, Cleanup, cleanupDirectories, getCleanupDirectories...

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

func (o *snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	var id string
	var info snapshots.Info
	if err := o.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		var usage snapshots.Usage
		var err error
		id, info, usage, err = storage.GetInfo(ctx, key)
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

func (o *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return o.createSnapshot(ctx, snapshots.KindView, key, parent, opts...)
}

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
