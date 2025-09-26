package fs

type FileMeta struct {
	FullPath   string
	LayerIndex int
}

type Node struct {
	FileMeta
}

type OverlayFS interface {
	// Merge merges the file with fileMeta from the specified layerIndex into the overlayfs view.
	// It returns true if the file was merged (i.e., it is relevant and not deleted or shadowed),
	// false if the file was not merged (i.e., it is deleted or shadowed by a whiteout or opaque parent).
	Merge(fileMeta FileMeta) (ok bool, err error)
	// HasWhiteoutFile(fileMeta FileMeta) (ok bool, layerIndex int, err error)
	// HasOpaqueParent(fileMeta FileMeta) (ok bool, layerIndex int, err error)
	Walk(walkingFunc func(node Node) error) error

	GetLayer(layerIndex int) string
}
