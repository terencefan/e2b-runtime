//go:build linux

package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"github.com/stretchr/testify/require"
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
