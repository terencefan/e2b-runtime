//go:build linux

package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestScanOverlayConflict(t *testing.T) {
	for _, tt := range []struct {
		name string
		h    tar.Header
		want bool
	}{
		{"ordinary", tar.Header{Name: "file", Mode: 0644, Typeflag: tar.TypeReg}, false},
		{"oci-whiteout", tar.Header{Name: ".wh.file", Mode: 0644, Typeflag: tar.TypeReg}, false},
		{"native-device", tar.Header{Name: "nested/device", Mode: 0600, Typeflag: tar.TypeChar}, true},
		{"other-device", tar.Header{Name: "dev/null", Mode: 0600, Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}, false},
		{"overlay-xattr", tar.Header{Name: "dir", Mode: 0755, Typeflag: tar.TypeDir, PAXRecords: map[string]string{"SCHILY.xattr.trusted.overlay.opaque": "y"}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			w := tar.NewWriter(&b)
			require.NoError(t, w.WriteHeader(&tt.h))
			require.NoError(t, w.Close())
			got, err := scanOverlayConflict(context.Background(), &b)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestSequentialLayersPreserveDeviceAndWhiteout(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "root")
	require.NoError(t, os.Mkdir(dest, 0755))
	var paths []string
	for i, headers := range [][]tar.Header{
		{{Name: "nested", Typeflag: tar.TypeDir, Mode: 0755}, {Name: "nested/device", Typeflag: tar.TypeChar, Mode: 0600}, {Name: "obsolete", Typeflag: tar.TypeReg, Mode: 0644}},
		{{Name: ".wh.obsolete", Typeflag: tar.TypeReg, Mode: 0644}, {Name: "new", Typeflag: tar.TypeReg, Mode: 0644}},
	} {
		p := filepath.Join(dir, fmt.Sprint(i))
		f, err := os.Create(p)
		require.NoError(t, err)
		w := tar.NewWriter(f)
		for j := range headers {
			require.NoError(t, w.WriteHeader(&headers[j]))
		}
		require.NoError(t, w.Close())
		require.NoError(t, f.Close())
		paths = append(paths, p)
	}
	require.NoError(t, applyOriginalLayers(t.Context(), paths, dest))
	st, err := os.Lstat(filepath.Join(dest, "nested/device"))
	require.NoError(t, err)
	require.NotZero(t, st.Mode()&os.ModeCharDevice)
	require.Equal(t, uint64(0), st.Sys().(*syscall.Stat_t).Rdev)
	_, err = os.Stat(filepath.Join(dest, "obsolete"))
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(dest, "new"))
	require.NoError(t, err)
}
