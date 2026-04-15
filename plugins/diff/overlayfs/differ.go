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
	"strings"
	"time"

	"github.com/containerd/continuity/fs"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/epoch"
	"github.com/containerd/containerd/v2/pkg/labels"
)

type overlayfsDiff struct {
	store content.Store
}

var emptyDesc = ocispec.Descriptor{}

// NewOverlayfsDiff is a generic implementation of diff.Comparer. The diff is
// calculated by mounting the lower mount set and comparing it against the
// overlay upper layer contents derived from the overlay mount options.
func NewOverlayfsDiff(store content.Store) diff.Comparer {
	return &overlayfsDiff{
		store: store,
	}
}

// Compare creates a diff between the given mounts and uploads the result
// to the content store.
func (s *overlayfsDiff) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (d ocispec.Descriptor, err error) {
	layer, err := overlayMountsToLayer(upper)
	if err != nil {
		return emptyDesc, fmt.Errorf("failed to get overlay layer: %w", err)
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

	upperRoot := filepath.Join(layer, "fs")
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
		errOpen = writeDiff(ctx, io.MultiWriter(compressed, dgstr.Hash()), lower, upperRoot)
		compressed.Close()
		if errOpen != nil {
			return emptyDesc, fmt.Errorf("failed to write compressed diff: %w", errOpen)
		}

		if config.Labels == nil {
			config.Labels = map[string]string{}
		}
		config.Labels[labels.LabelUncompressed] = dgstr.Digest().String()
	} else {
		if err := writeDiff(ctx, cw, lower, upperRoot); err != nil {
			return emptyDesc, fmt.Errorf("failed to create diff tar stream: %w", err)
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

	return ocispec.Descriptor{
		MediaType: config.MediaType,
		Size:      info.Size,
		Digest:    info.Digest,
	}, nil
}

func uniqueRef() string {
	t := time.Now()
	var b [3]byte
	// Ignore read failures, just decreases uniqueness
	rand.Read(b[:])
	return fmt.Sprintf("%d-%s", t.UnixNano(), base64.URLEncoding.EncodeToString(b[:]))
}

func writeDiff(ctx context.Context, w io.Writer, lower []mount.Mount, upperRoot string) error {
	var opts []archive.ChangeWriterOpt

	return mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
		cw := archive.NewChangeWriter(w, upperRoot, opts...)
		if err := fs.DiffDirChanges(ctx, lowerRoot, upperRoot, fs.DiffSourceOverlayFS, cw.HandleChange); err != nil {
			return fmt.Errorf("failed to create diff tar stream: %w", err)
		}
		return cw.Close()
	})
}

// overlayMountsToLayer extracts the overlay layer root from the mount options.
// It expects the first mount to be of type "overlay" and derives the layer from
// upperdir when present, otherwise from the top-most lowerdir entry.
func overlayMountsToLayer(mounts []mount.Mount) (string, error) {
	if len(mounts) == 0 {
		return "", errors.New("no mounts provided")
	}
	if mounts[0].Type != "overlay" {
		return "", fmt.Errorf("expected overlay mount type, got %s", mounts[0].Type)
	}

	mnt := mounts[0]
	var layer string
	var topLower string
	for _, o := range mnt.Options {
		if k, v, ok := strings.Cut(o, "="); ok {
			switch k {
			case "upperdir":
				layer = filepath.Dir(v)
			case "lowerdir":
				dir, _, _ := strings.Cut(v, ":")
				topLower = filepath.Dir(dir)
			}
		}
	}

	if layer == "" {
		if topLower == "" {
			return "", fmt.Errorf("unsupported overlay layer for overlayfs differ: %w", errdefs.ErrNotImplemented)
		}
		layer = topLower
	}
	return layer, nil
}
