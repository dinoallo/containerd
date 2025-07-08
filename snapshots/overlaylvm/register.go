package overlaylvm

import (
	"github.com/containerd/containerd/snapshots"
)

func RegisterSnapshotter(root, vgName, lvSize, mountParent string) (snapshots.Snapshotter, error) {
	return NewSnapshotter(root, vgName, lvSize, mountParent)
}
