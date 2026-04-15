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

package plugin

import (
	"errors"
	"fmt"

	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/containerd/v2/plugins/snapshots/devbox"
	"github.com/containerd/platforms"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
)

// Config represents configuration for the devbox snapshotter plugin.
type Config struct {
	// RootPath overrides the default plugin root directory.
	RootPath string `toml:"root_path"`

	// UpperdirLabel enables exporting the overlay upperdir label.
	UpperdirLabel bool `toml:"upperdir_label"`

	// SyncRemove disables asynchronous removal when set to true.
	SyncRemove bool `toml:"sync_remove"`

	// LvmVgName is the LVM volume group used by the devbox snapshotter.
	LvmVgName string `toml:"lvm_vg_name"`

	// ThinPoolName is the thin pool name used by the devbox snapshotter.
	ThinPoolName string `toml:"thin_pool_name"`

	// MountOptions are default mount options used for overlay mounts.
	MountOptions []string `toml:"mount_options"`
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   plugins.SnapshotPlugin,
		ID:     "devbox",
		Config: &Config{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ic.Meta.Platforms = append(ic.Meta.Platforms, platforms.DefaultSpec())

			config, ok := ic.Config.(*Config)
			if !ok {
				return nil, errors.New("invalid devbox configuration")
			}

			root := ic.Properties[plugins.PropertyRootDir]
			if config.RootPath != "" {
				root = config.RootPath
			}
			if root == "" {
				return nil, fmt.Errorf("devbox root path is empty")
			}

			var opts []devbox.Opt
			if config.UpperdirLabel {
				opts = append(opts, devbox.WithUpperdirLabel)
			}
			if !config.SyncRemove {
				opts = append(opts, devbox.AsynchronousRemove)
			}
			if len(config.MountOptions) > 0 {
				opts = append(opts, devbox.WithMountOptions(config.MountOptions))
			}
			if config.LvmVgName != "" {
				opts = append(opts, devbox.WithLvmVgName(config.LvmVgName))
			}
			if config.ThinPoolName != "" {
				opts = append(opts, devbox.WithThinPoolName(config.ThinPoolName))
			}

			ic.Meta.Exports[plugins.SnapshotterRootDir] = root
			return devbox.NewSnapshotter(root, opts...)
		},
	})
}
