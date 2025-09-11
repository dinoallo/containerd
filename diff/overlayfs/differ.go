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

package overlayfs

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/containerd/containerd/diff/walking"
	"github.com/containerd/log"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/archive"
	"github.com/containerd/containerd/archive/compression"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	fs "github.com/containerd/containerd/diff/overlayfs/pkg/fs"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/labels"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/pkg/epoch"
)

type overlayfsDiff struct {
	store     content.Store
	naiveDiff diff.Comparer
}

var emptyDesc = ocispec.Descriptor{}

// NewOverlayfsDiff is a generic implementation of diff.Comparer.  The diff is
// calculated by mounting both the upper and lower mount sets and walking the
// mounted directories concurrently. Changes are calculated by comparing files
// against each other or by comparing file existence between directories.
// NewOverlayfsDiff uses no special characteristics of the mount sets and is
// expected to work with any filesystem.
func NewOverlayfsDiff(store content.Store) diff.Comparer {
	return &overlayfsDiff{
		store:     store,
		naiveDiff: walking.NewWalkingDiff(store),
	}
}

// Compare creates a diff between the given mounts and uploads the result
// to the content store.
func (s *overlayfsDiff) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (d ocispec.Descriptor, err error) {
	// Try to generate diff layers based on overlayfs mount options
	// If successful, we can generate the diff more efficiently
	// by only looking at the diff layers instead of doing a full
	// directory walk.
	diffLayers, err := tryGeneratingDiffLayers(lower, upper)
	if err != nil {
		return emptyDesc, fmt.Errorf("failed to generate diff layers: %w", err)
	}
	// upper is not based on lower, or upper is the same as lower. In this case, we will do a naive diff
	if len(diffLayers) == 0 {
		return s.naiveDiff.Compare(ctx, lower, upper, opts...)
	}
	var config diff.Config
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return emptyDesc, err
		}
	}
	if tm := epoch.FromContext(ctx); tm != nil && config.SourceDateEpoch == nil {
		config.SourceDateEpoch = tm
	}

	// if config.MediaType is not set, we default to gzip compressed layer
	if config.MediaType == "" {
		config.MediaType = ocispec.MediaTypeImageLayerGzip
	}

	var compressionType compression.Compression
	switch config.MediaType {
	case ocispec.MediaTypeImageLayer:
		compressionType = compression.Uncompressed
	case ocispec.MediaTypeImageLayerGzip:
		compressionType = compression.Gzip
	case ocispec.MediaTypeImageLayerZstd:
		compressionType = compression.Zstd
	default:
		return emptyDesc, fmt.Errorf("unsupported diff media type: %v: %w", config.MediaType, errdefs.ErrNotImplemented)
	}

	for i, mount := range lower {
		log.L.Debugf("no. %v lower mount source: %v", i, mount.Source)
		log.L.Debugf("no. %v lower mount target: %v", i, mount.Target)
		log.L.Debugf("no. %v lower mount options: %v", i, mount.Options)
		log.L.Debugf("no. %v lower mount type: %v", i, mount.Type)
	}
	for i, mount := range upper {
		log.L.Debugf("no. %v upper mount source: %v", i, mount.Source)
		log.L.Debugf("no. %v upper mount target: %v", i, mount.Target)
		log.L.Debugf("no. %v upper mount options: %v", i, mount.Options)
		log.L.Debugf("no. %v upper mount type: %v", i, mount.Type)
	}

	var ocidesc ocispec.Descriptor
	var newReference bool
	if config.Reference == "" {
		newReference = true
		config.Reference = uniqueRef()
	}

	cw, err := s.store.Writer(ctx,
		content.WithRef(config.Reference),
		content.WithDescriptor(ocispec.Descriptor{
			MediaType: config.MediaType, // most contentstore implementations just ignore this
		}))
	if err != nil {
		return emptyDesc, fmt.Errorf("failed to open writer: %w", err)
	}

	// errOpen is set when an error occurs while the content writer has not been
	// committed or closed yet to force a cleanup
	var errOpen error
	defer func() {
		if errOpen != nil {
			cw.Close()
			if newReference {
				if abortErr := s.store.Abort(ctx, config.Reference); abortErr != nil {
					log.G(ctx).WithError(abortErr).WithField("ref", config.Reference).Warnf("failed to delete diff upload")
				}
			}
		}
	}()
	if !newReference {
		if errOpen = cw.Truncate(0); errOpen != nil {
			return emptyDesc, errOpen
		}
	}

	if compressionType != compression.Uncompressed {
		dgstr := digest.SHA256.Digester()
		var compressed io.WriteCloser
		if config.Compressor != nil {
			compressed, errOpen = config.Compressor(cw, config.MediaType)
			if errOpen != nil {
				return emptyDesc, fmt.Errorf("failed to get compressed stream: %w", errOpen)
			}
		} else {
			compressed, errOpen = compression.CompressStream(cw, compressionType)
			if errOpen != nil {
				return emptyDesc, fmt.Errorf("failed to get compressed stream: %w", errOpen)
			}
		}
		errOpen = writeDiff(ctx, io.MultiWriter(compressed, dgstr.Hash()), lower, diffLayers, config.SourceDateEpoch)
		compressed.Close()
		if errOpen != nil {
			return emptyDesc, fmt.Errorf("failed to write compressed diff: %w", errOpen)
		}

		if config.Labels == nil {
			config.Labels = map[string]string{}
		}
		config.Labels[labels.LabelUncompressed] = dgstr.Digest().String()
	} else {
		err := writeDiff(ctx, cw, lower, diffLayers, config.SourceDateEpoch)
		if err != nil {
			return emptyDesc, fmt.Errorf("failed to write diff: %w", err)
		}
	}

	var commitopts []content.Opt
	if config.Labels != nil {
		commitopts = append(commitopts, content.WithLabels(config.Labels))
	}

	dgst := cw.Digest()
	if errOpen = cw.Commit(ctx, 0, dgst, commitopts...); errOpen != nil {
		if !errdefs.IsAlreadyExists(errOpen) {
			return emptyDesc, fmt.Errorf("failed to commit: %w", errOpen)
		}
		errOpen = nil
	}

	info, err := s.store.Info(ctx, dgst)
	if err != nil {
		return emptyDesc, fmt.Errorf("failed to get info from content store: %w", err)
	}
	if info.Labels == nil {
		info.Labels = make(map[string]string)
	}
	// Set "containerd.io/uncompressed" label if digest already existed without label
	if _, ok := info.Labels[labels.LabelUncompressed]; !ok {
		info.Labels[labels.LabelUncompressed] = config.Labels[labels.LabelUncompressed]
		if _, err := s.store.Update(ctx, info, "labels."+labels.LabelUncompressed); err != nil {
			return emptyDesc, fmt.Errorf("error setting uncompressed label: %w", err)
		}
	}

	ocidesc = ocispec.Descriptor{
		MediaType: config.MediaType,
		Size:      info.Size,
		Digest:    info.Digest,
	}

	return ocidesc, nil
}

func uniqueRef() string {
	t := time.Now()
	var b [3]byte
	// Ignore read failures, just decreases uniqueness
	rand.Read(b[:])
	return fmt.Sprintf("%d-%s", t.UnixNano(), base64.URLEncoding.EncodeToString(b[:]))
}

func writeDiff(ctx context.Context, w io.Writer, lower []mount.Mount, diffLayers []string, sourceDateEpoch *time.Time) error {
	var opts []archive.ChangeWriterOpt
	if sourceDateEpoch != nil {
		opts = append(opts, archive.WithModTimeUpperBound(*sourceDateEpoch))
	}

	return mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
		changeFns := make([]fs.ChangeFunc, 0, len(diffLayers))
		changeWriters := make([]*archive.ChangeWriter, 0, len(diffLayers))
		for _, l := range diffLayers {
			root := filepath.Join(l, "fs")
			cw := archive.NewChangeWriter(w, root, opts...)
			changeFns = append(changeFns, cw.HandleChange)
			changeWriters = append(changeWriters, cw)
		}
		if err := fs.DiffDirChanges(lowerRoot, diffLayers, changeFns); err != nil {
			return fmt.Errorf("failed to calculate diff changes: %w", err)
		}
		for _, cw := range changeWriters {
			if err := cw.Close(); err != nil {
				return fmt.Errorf("failed to close change writer: %w", err)
			}
		}
		return nil
	})
}

func overlayMountsToLayers(mounts []mount.Mount) ([]string, error) {
	if len(mounts) == 0 {
		return nil, errors.New("no mounts provided")
	}
	if mounts[0].Type != "overlay" {
		return nil, fmt.Errorf("expected overlay mount type, got %s", mounts[0].Type)
	}
	var (
		mnt    = mounts[0]
		layers = []string{}
	)

	for _, o := range mnt.Options {
		if k, v, ok := strings.Cut(o, "="); ok {
			switch k {
			case "upperdir":
				layers = append(layers, filepath.Dir(v))
			case "lowerdir":
				// lowerdir can be a colon-separated list
				for _, dir := range strings.Split(v, ":") {
					layers = append(layers, filepath.Dir(dir))
				}
			}
		}
	}
	if len(layers) == 0 {
		return nil, fmt.Errorf("no layers found in overlay mount options")
	}
	slices.Reverse(layers)
	return layers, nil
}

// tryGeneratingDiffLayers tries to determine the diff layers between two overlayfs mount sets.
// If the child mount is based on the ancestor mount, it returns the diff layers.
// If the child mount is not based on the ancestor mount, it returns nil.
// If the child mount is the same as the ancestor mount, it returns an empty slice.
// If there is an error, it returns an error.
func tryGeneratingDiffLayers(child, ancestor []mount.Mount) ([]string, error) {
	var (
		diffLayers = []string{}
	)
	// Get the layers of the child from overlay mount options
	childLayers, err := overlayMountsToLayers(child)
	if err != nil {
		return nil, fmt.Errorf("failed to get layers from overlayfs1: %w", err)
	}
	// Get the layers of the ancestor from overlay mount options
	ancestorLayers, err := overlayMountsToLayers(ancestor)
	if err != nil {
		return nil, fmt.Errorf("failed to get layers from overlayfs2: %w", err)
	}
	// The child is not based on ancestor, or child is the same as ancestor
	if len(childLayers) < len(ancestorLayers) {
		return nil, nil
	}
	for i, l := range ancestorLayers {
		if l != childLayers[i] {
			// The ancestor is not based on the child
			log.L.Debugf("child layer %s is not equal to ancestor layer %s", childLayers[i], l)
			return nil, nil
		}
	}
	if len(childLayers) > len(ancestorLayers) {
		// The child is based on the ancestor
		// Get the diff layers
		diffLayers = childLayers[len(ancestorLayers):]
	}
	return diffLayers, nil
}
