//go:build linux

package oci

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/google/go-containerregistry/pkg/name"
	containerregistry "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/moby/go-archive"
	"github.com/moby/go-archive/chrootarchive"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/core/filesystem"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/core/oci/auth"
	"github.com/e2b-dev/infra/packages/shared/pkg/dockerhub"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/units"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// ErrMountCleanup indicates build files may still be in use by a mount.
var ErrMountCleanup = errors.New("template mount cleanup failed")

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/core/oci")

// ImageTooLargeError is returned when the uncompressed Docker image exceeds the maximum filesystem size.
type ImageTooLargeError struct {
	ImageSize int64 // actual uncompressed size in bytes, 0 if unknown
	MaxSize   int64 // maximum filesystem size in bytes
}

func (e *ImageTooLargeError) Error() string {
	if e.ImageSize > 0 {
		return fmt.Sprintf(
			"the uncompressed Docker image size (%s) exceeds the maximum filesystem size (%s). "+
				"Please reduce your Docker image size (e.g., use a smaller base image, multi-stage builds, or remove unnecessary files)",
			humanize.Bytes(uint64(e.ImageSize)),
			humanize.Bytes(uint64(e.MaxSize)),
		)
	}

	return fmt.Sprintf(
		"the Docker image is too large for the maximum filesystem size of %s. "+
			"Please reduce your Docker image size (e.g., use a smaller base image, multi-stage builds, or remove unnecessary files)",
		humanize.Bytes(uint64(e.MaxSize)),
	)
}

// DefaultPlatform returns the OCI platform for image pulls, respecting TARGET_ARCH.
func DefaultPlatform() containerregistry.Platform {
	return containerregistry.Platform{
		OS:           "linux",
		Architecture: utils.TargetArch(),
	}
}

// wrapImagePullError converts technical Docker registry errors into user-friendly messages.
func wrapImagePullError(ctx context.Context, err error, imageRef string) error {
	if err == nil {
		return nil
	}

	proxyAccessDenied := errors.Is(err, dockerhub.ErrRepositoryAccessDenied)
	logger.L().Warn(ctx, "failed to pull image", zap.String("image_ref", imageRef), zap.Bool("dockerhub_proxy_access_denied", proxyAccessDenied), zap.Error(err))

	if proxyAccessDenied {
		// Keep the classification without exposing registry-controlled error details.
		return fmt.Errorf("failed to pull image '%s': %w", imageRef, dockerhub.ErrRepositoryAccessDenied)
	}

	// Check for transport errors with specific error codes from the registry API
	if transportErr, ok := errors.AsType[*transport.Error](err); ok {
		for _, e := range transportErr.Errors {
			switch e.Code {
			case transport.ManifestUnknownErrorCode:
				return fmt.Errorf("image '%s' not found: the image or tag does not exist in the registry", imageRef)
			case transport.NameUnknownErrorCode:
				return fmt.Errorf("repository '%s' not found: verify the image name is correct", imageRef)
			case transport.UnauthorizedErrorCode:
				return fmt.Errorf("access denied to '%s': authentication required or insufficient permissions", imageRef)
			case transport.DeniedErrorCode:
				return fmt.Errorf("access denied to '%s': you don't have permission to pull this image", imageRef)
			}
		}

		if transportErr.StatusCode != 0 {
			return fmt.Errorf("failed to pull image '%s': registry returned status code %d", imageRef, transportErr.StatusCode)
		}
	}

	return fmt.Errorf("failed to pull image '%s': unable to retrieve image from registry", imageRef)
}

func GetPublicImage(ctx context.Context, dockerhubRepository dockerhub.RemoteRepository, tag string, authProvider auth.RegistryAuthProvider) (containerregistry.Image, error) {
	ctx, span := tracer.Start(ctx, "pull-public-docker-image")
	defer span.End()

	ref, err := name.ParseReference(tag)
	if err != nil {
		return nil, fmt.Errorf("invalid image reference '%s': %w", tag, err)
	}

	platform := DefaultPlatform()

	// When no auth provider is provided and the image is from the default registry
	// use docker remote repository proxy with cached images
	if authProvider == nil && ref.Context().RegistryStr() == name.DefaultRegistry {
		img, err := dockerhubRepository.GetImage(ctx, tag, platform)
		if err != nil {
			return nil, wrapImagePullError(ctx, err, tag)
		}

		telemetry.ReportEvent(ctx, "pulled public image through docker remote repository proxy")

		err = verifyImagePlatform(ctx, img, platform, tag)
		if err != nil {
			return nil, err
		}

		return img, nil
	}

	// Build options
	opts := []remote.Option{remote.WithPlatform(platform)}

	// Use the auth provider if provided
	if authProvider != nil {
		authOption, err := authProvider.GetAuthOption(ctx)
		if err != nil {
			return nil, fmt.Errorf("error getting auth option: %w", err)
		}
		if authOption != nil {
			opts = append(opts, authOption)
		}
	}

	img, err := remote.Image(ref, opts...)
	if err != nil {
		return nil, wrapImagePullError(ctx, err, tag)
	}

	telemetry.ReportEvent(ctx, "pulled public image")

	err = verifyImagePlatform(ctx, img, platform, tag)
	if err != nil {
		return nil, err
	}

	return img, nil
}

func GetImageSize(img containerregistry.Image) (int64, error) {
	imageSize := int64(0)

	layers, err := img.Layers()
	if err != nil {
		return 0, fmt.Errorf("error getting image layers: %w", err)
	}

	for index, layer := range layers {
		layerSize, err := layer.Size()
		if err != nil {
			return 0, fmt.Errorf("error getting layer (%d) size: %w", index, err)
		}
		imageSize += layerSize
	}

	return imageSize, nil
}

func ToExt4(ctx context.Context, logger logger.Logger, img containerregistry.Image, rootfsPath string, maxSize int64, blockSize int64) (int64, error) {
	ctx, childSpan := tracer.Start(ctx, "oci-to-ext4")
	defer childSpan.End()

	err := filesystem.Make(ctx, rootfsPath, units.BytesToMB(maxSize), blockSize)
	if err != nil {
		return 0, fmt.Errorf("error creating ext4 file: %w", err)
	}

	err = ExtractToExt4(ctx, logger, img, rootfsPath, maxSize)
	if err != nil {
		return 0, fmt.Errorf("error extracting image to ext4 filesystem: %w", err)
	}

	// Check the FS integrity first so no errors occur during shrinking
	_, err = filesystem.CheckIntegrity(ctx, rootfsPath, true)
	if err != nil {
		return 0, fmt.Errorf("error checking filesystem integrity after ext4 creation: %w", err)
	}

	// The filesystem is first created with the maximum size, so we need to shrink it to the actual size
	size, err := filesystem.Shrink(ctx, rootfsPath)
	if err != nil {
		return 0, fmt.Errorf("error shrinking ext4 filesystem: %w", err)
	}

	// Check the FS integrity after shrinking
	_, err = filesystem.CheckIntegrity(ctx, rootfsPath, true)
	if err != nil {
		return 0, fmt.Errorf("error checking filesystem integrity after shrinking: %w", err)
	}

	return size, nil
}

func ExtractToExt4(ctx context.Context, l logger.Logger, img containerregistry.Image, rootfsPath string, maxSize int64) (err error) {
	ctx, childSpan := tracer.Start(ctx, "extract-to-ext4")
	defer childSpan.End()

	ociPath, err := os.MkdirTemp(filepath.Dir(rootfsPath), ".oci-image-")
	if err != nil {
		return fmt.Errorf("create OCI staging directory: %w", err)
	}
	defer func() {
		if !errors.Is(err, ErrMountCleanup) {
			if cleanupErr := os.RemoveAll(ociPath); cleanupErr != nil {
				logger.L().Error(ctx, "remove OCI staging directory", zap.Error(cleanupErr))
			}
		}
	}()
	localImage, originals, sequential, err := stageOriginalLayers(ctx, img, ociPath)
	if err != nil {
		return fmt.Errorf("stage original OCI layers: %w", err)
	}
	if sequential {
		l.Info(ctx, "materialization=sequential reason=overlay-control-content")
		return runMountWorker(ctx, l, mountRequest{Rootfs: rootfsPath, OriginalLayers: originals, MaxSize: maxSize})
	}
	l.Info(ctx, "materialization=overlay")
	layers, err := createExport(ctx, l, localImage, ociPath)
	if err != nil {
		return fmt.Errorf("export OCI layers: %w", err)
	}
	return runMountWorker(ctx, l, mountRequest{Rootfs: rootfsPath, Layers: layers, MaxSize: maxSize})
}

func populateExt4(ctx context.Context, request mountRequest, result *mountResult) (err error) {
	rootfsPath := request.Rootfs

	tempRoot := filepath.Dir(rootfsPath)
	tmpMount, err := os.MkdirTemp(tempRoot, ".ext4-mount-")
	if err != nil {
		return fmt.Errorf("error creating temporary mount point: %w", err)
	}
	defer func() {
		if errors.Is(err, ErrMountCleanup) {
			return
		}
		if removeErr := os.Remove(tmpMount); removeErr != nil {
			logger.L().Error(ctx, "error removing temporary mount point", zap.Error(removeErr))
		}
	}()

	err = filesystem.Mount(ctx, rootfsPath, tmpMount)
	if err != nil {
		return fmt.Errorf("%w: mount ext4: %w", ErrMountCleanup, err)
	}
	defer func() {
		err = errors.Join(err, unmountBuildFilesystem(ctx, "ext4", tmpMount))
	}()

	logger.L().Debug(ctx, "extracting image to ext4 filesystem",
		zap.String("rootfs_path", rootfsPath),
		zap.String("tmp_mount", tmpMount),
	)

	if len(request.OriginalLayers) > 0 {
		return applyOriginalLayers(ctx, request.OriginalLayers, tmpMount)
	}
	return mountAndCopy(ctx, request.Layers, tmpMount, tempRoot, request.MaxSize, result)
}

func unmountBuildFilesystem(ctx context.Context, filesystemType, mountpoint string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := filesystem.Unmount(cleanupCtx, mountpoint); err != nil {
		namespace, _ := os.Readlink("/proc/self/ns/mnt")
		mountInfo, _ := os.ReadFile("/proc/self/mountinfo")
		var entries []string
		for line := range strings.SplitSeq(string(mountInfo), "\n") {
			if strings.Contains(line, mountpoint) {
				entries = append(entries, line)
			}
		}
		return fmt.Errorf("%w: unmount %s at %s (namespace %s, mountinfo %q): %w", ErrMountCleanup, filesystemType, mountpoint, namespace, entries, err)
	}
	return nil
}

func ParseEnvs(envs []string) map[string]string {
	envMap := make(map[string]string, len(envs))
	for _, env := range envs {
		if strings.TrimSpace(env) == "" {
			continue
		}
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key != "" && value != "" {
			envMap[key] = value
		}
	}

	return envMap
}

func mountAndCopy(ctx context.Context, layers []string, destDir, tempRoot string, maxSize int64, result *mountResult) (err error) {
	// Mount the overlay filesystem with the extracted layers
	mountPath, err := os.MkdirTemp(tempRoot, ".overlayfs-mount-")
	if err != nil {
		return fmt.Errorf("while creating temporary file for squashed image: %w", err)
	}
	defer func() {
		if errors.Is(err, ErrMountCleanup) {
			return
		}
		if removeErr := os.Remove(mountPath); removeErr != nil {
			logger.L().Error(ctx, "error removing temporary overlayfs mount point", zap.Error(removeErr))
		}
	}()

	err = filesystem.MountOverlayFS(ctx, layers, mountPath)
	if err != nil {
		return fmt.Errorf("%w: mount overlayfs: %w", ErrMountCleanup, err)
	}
	defer func() {
		err = errors.Join(err, unmountBuildFilesystem(ctx, "overlayfs", mountPath))
	}()

	// List files in the mount point
	files, err := listFiles(ctx, mountPath)
	if err != nil {
		return fmt.Errorf("while listing files in overlayfs: %w", err)
	}
	result.RootEntries = files

	// Copy files from the overlayfs mount point to the destination directory
	err = copyFiles(ctx, mountPath, destDir)
	if err != nil {
		if strings.Contains(err.Error(), "No space left on device") {
			imageSize, _ := getDirSize(ctx, mountPath)

			return &ImageTooLargeError{ImageSize: imageSize, MaxSize: maxSize}
		}

		return fmt.Errorf("while copying files from overlayfs to destination directory: %w", err)
	}

	return nil
}

func listFiles(ctx context.Context, dir string) ([]string, error) {
	_, childSpan := tracer.Start(ctx, "list-files")
	defer childSpan.End()

	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("while reading directory %s: %w", dir, err)
	}

	return utils.Map(files, func(file os.DirEntry) string {
		return file.Name()
	}), nil
}

// copyFiles uses rsync to copy files from the source directory to the destination directory.
func copyFiles(ctx context.Context, src, dest string) error {
	_, childSpan := tracer.Start(ctx, "copy-files")
	defer childSpan.End()

	// Does the following:
	// Recursion into directories
	// Symlinks
	// Permissions
	// Modification times
	// Group/owner (if possible)
	// Device files and special files
	// Hard links (-H)
	//
	// --whole-file: Copy files without using the delta algorithm, which is faster for local copies
	// --inplace: Update destination files in place, no need to create temporary files
	cmd := exec.CommandContext(ctx, "rsync", "-aH", "--whole-file", "--inplace", src+"/", dest)
	// Pin rsync's working directory to root. When template-manager is exec'd
	// via `nsenter -t 1 -m -u -i -n -w/` into PID 1's namespaces, a child
	// process's inherited CWD can land on a directory that has been unmounted
	// in another mount namespace, causing rsync's receiver to abort at
	// startup with `getcwd(): No such file or directory (2)`. `/` is always
	// present in the host mount namespace, so anchoring there is safe.
	cmd.Dir = "/"
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("while copying files from %s to %s: %w: %s", src, dest, err, string(out))
	}

	return nil
}

// createExport extracts the layers of the source image into a temporary directory
// and returns the paths of the extracted layers. The layers are extracted in reverse order
// to maintain the correct order for overlayFS.
// The layers are extracted in parallel to speed up the process.
func createExport(ctx context.Context, logger logger.Logger, srcImage containerregistry.Image, path string) ([]string, error) {
	ctx, childSpan := tracer.Start(ctx, "create-oci-export")
	defer childSpan.End()

	layers, err := srcImage.Layers()
	if err != nil {
		return nil, fmt.Errorf("while getting layers of source image: %w", err)
	}

	layerPaths := make([]string, len(layers))
	var eg errgroup.Group
	// Always join started extractors before the caller can remove staging files.
	defer func() { _ = eg.Wait() }()
	for i, l := range layers {
		digest, err := l.Digest()
		if err != nil {
			return nil, fmt.Errorf("failed to get digest of layer %d: %w", i, err)
		}
		size, err := l.Size()
		if err != nil {
			return nil, fmt.Errorf("failed to get size of layer %d: %w", i, err)
		}
		telemetry.ReportEvent(ctx, "uncompressing layer",
			attribute.Int("layer.index", i),
			attribute.String("layer.digest", digest.String()),
			attribute.Int64("layer.size", size),
		)
		logger.Info(ctx, fmt.Sprintf("Uncompressing layer %s %s", digest, humanize.Bytes(uint64(size))))

		// Each layer has to be uniquely named, even if the digest is the same across different layers
		layerPath := filepath.Join(path, fmt.Sprintf("layer-%d-%s", i, strings.ReplaceAll(digest.String(), ":", "-")))
		// Layers need to be reported in reverse order to maintain the correct layer order for overlayfs
		layerPaths[len(layers)-i-1] = layerPath
		eg.Go(func() error {
			err := os.MkdirAll(layerPath, 0o755)
			if err != nil {
				return fmt.Errorf("failed to create directory for layer %d: %w", i, err)
			}

			rc, err := l.Uncompressed()
			if err != nil {
				return fmt.Errorf("failed to get uncompressed layer %d: %w", i, err)
			}
			defer rc.Close()
			// Use chrootarchive (same mechanism as the Docker daemon's layer unpacking).
			// Note: layers with redundant nested whiteouts (.wh.foo + foo/.wh.bar)
			// are not supported and fail here with ENOTDIR — same as stock Docker
			// (both overlay2 and containerd snapshotter reject such images).
			err = chrootarchive.UntarUncompressed(rc, layerPath, &archive.TarOptions{
				WhiteoutFormat: archive.OverlayWhiteoutFormat,
			})
			if err != nil {
				return fmt.Errorf("failed to untar layer %d: %w", i, err)
			}

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, fmt.Errorf("while extracting layers: %w", err)
	}

	logger.Info(ctx, "Layers extracted")

	return layerPaths, nil
}

// getDirSize returns the total disk usage of a directory in bytes using du -sb.
func getDirSize(ctx context.Context, dir string) (int64, error) {
	cmd := exec.CommandContext(ctx, "du", "-sb", dir)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("error running du: %w", err)
	}

	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return 0, fmt.Errorf("unexpected du output: %s", string(out))
	}

	size, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("error parsing du output: %w", err)
	}

	return size, nil
}

func verifyImagePlatform(ctx context.Context, img containerregistry.Image, platform containerregistry.Platform, imageRef string) error {
	config, err := img.ConfigFile()
	if err != nil {
		logger.L().Warn(ctx, "failed to get image config file", zap.String("image_ref", imageRef), zap.Error(err))

		return fmt.Errorf("failed to inspect image '%s': unable to retrieve image metadata from registry", imageRef)
	}
	if config.Architecture != platform.Architecture {
		return fmt.Errorf("image architecture %q does not match expected %q", config.Architecture, platform.Architecture)
	}

	return nil
}
