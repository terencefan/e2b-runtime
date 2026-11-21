//go:build linux

package oci

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/moby/go-archive"
	"github.com/moby/go-archive/chrootarchive"
	"github.com/moby/go-archive/compression"
)

func scanOverlayConflict(ctx context.Context, r io.Reader) (bool, error) {
	tr := tar.NewReader(r)
	conflict := false
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		h, err := tr.Next()
		if err == io.EOF {
			return conflict, nil
		}
		if err != nil {
			return false, err
		}
		if h.Typeflag == tar.TypeChar && h.Devmajor == 0 && h.Devminor == 0 {
			conflict = true
		}
		for k := range h.PAXRecords {
			if strings.HasPrefix(k, "SCHILY.xattr.trusted.overlay.") || strings.HasPrefix(k, "SCHILY.xattr.user.overlay.") {
				conflict = true
			}
		}
	}
}

// Retain original compressed layers, before conversion of OCI whiteouts.
func stageOriginalLayers(ctx context.Context, img v1.Image, dir string) (v1.Image, []string, bool, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, nil, false, err
	}
	var local []v1.Layer
	var paths []string
	sequential := false
	for i, l := range layers {
		if err := ctx.Err(); err != nil {
			return nil, nil, false, err
		}
		digest, err := l.Digest()
		if err != nil {
			return nil, nil, false, err
		}
		if digest.Algorithm != "sha256" {
			return nil, nil, false, fmt.Errorf("unsupported layer digest algorithm %s", digest.Algorithm)
		}
		path := filepath.Join(dir, fmt.Sprintf("source-%d.blob", i))
		f, err := os.Create(path)
		if err != nil {
			return nil, nil, false, err
		}
		r, err := l.Compressed()
		if err != nil {
			f.Close()
			return nil, nil, false, err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(f, hash), r)
		closeRead := r.Close()
		closeWrite := f.Close()
		if copyErr != nil {
			return nil, nil, false, copyErr
		}
		if closeRead != nil {
			return nil, nil, false, closeRead
		}
		if closeWrite != nil {
			return nil, nil, false, closeWrite
		}
		if fmt.Sprintf("%x", hash.Sum(nil)) != digest.Hex {
			return nil, nil, false, fmt.Errorf("layer %d digest mismatch", i)
		}
		layer, err := tarball.LayerFromFile(path)
		if err != nil {
			return nil, nil, false, err
		}
		stream, err := layer.Uncompressed()
		if err != nil {
			return nil, nil, false, err
		}
		conflict, scanErr := scanOverlayConflict(ctx, stream)
		closeErr := stream.Close()
		if scanErr != nil {
			return nil, nil, false, scanErr
		}
		if closeErr != nil {
			return nil, nil, false, closeErr
		}
		sequential = sequential || conflict
		local = append(local, layer)
		paths = append(paths, path)
	}
	image, err := mutate.AppendLayers(empty.Image, local...)
	return image, paths, sequential, err
}

func applyOriginalLayers(ctx context.Context, paths []string, dest string) error {
	for i, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		r, err := compression.DecompressStream(f)
		if err != nil {
			f.Close()
			return err
		}
		_, applyErr := chrootarchive.ApplyUncompressedLayer(dest, r, &archive.TarOptions{})
		closeErr := r.Close()
		fileErr := f.Close()
		if applyErr != nil {
			return fmt.Errorf("apply OCI layer %d: %w", i, applyErr)
		}
		if closeErr != nil {
			return closeErr
		}
		if fileErr != nil {
			return fileErr
		}
	}
	return nil
}
