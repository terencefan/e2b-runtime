//go:build linux

package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

const mountWorkerArg = "__e2b_template_mount_worker"

type mountRequest struct {
	Rootfs  string
	Layers  []string
	MaxSize int64
}

type mountResult struct {
	Error       string
	Cleanup     bool
	TooLarge    *ImageTooLargeError
	RootEntries []string
}

// Dispatch in the package so orchestrator, create-build, and tests can all
// invoke the same worker without entering their normal startup paths.
func init() {
	if len(os.Args) == 2 && os.Args[1] == mountWorkerArg {
		os.Exit(mountWorkerMain())
	}
}

func mountWorkerMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	var request mountRequest
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	selfNS, selfErr := os.Readlink("/proc/self/ns/mnt")
	parentNS, parentErr := os.Readlink(fmt.Sprintf("/proc/%d/ns/mnt", os.Getppid()))
	if selfErr != nil || parentErr != nil || selfNS == parentNS {
		fmt.Fprintln(os.Stderr, "mount worker requires a separate mount namespace")
		return 1
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		fmt.Fprintln(os.Stderr, "make mount namespace private:", err)
		return 1
	}

	var result mountResult
	if err := populateExt4(ctx, request, &result); err != nil {
		result.Error = err.Error()
		result.Cleanup = errors.Is(err, ErrMountCleanup)
		if !result.Cleanup {
			errors.As(err, &result.TooLarge)
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runMountWorker(ctx context.Context, buildLogger logger.Logger, request mountRequest) error {
	input, err := json.Marshal(request)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "/proc/self/exe", mountWorkerArg)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS, Setpgid: true, Pdeathsig: syscall.SIGTERM}
	cmd.Stdin = bytes.NewReader(input)
	var output, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &stderr
	// Two sequential unmounts can each take ten seconds.
	cmd.WaitDelay = 22 * time.Second
	var terminate *time.Timer
	var terminationDone chan struct{}
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		terminationDone = make(chan struct{})
		terminate = time.AfterFunc(21*time.Second, func() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			close(terminationDone)
		})
		return nil
	}
	runErr := cmd.Run()
	if terminate != nil && !terminate.Stop() {
		<-terminationDone
	}

	var result mountResult
	decodeErr := json.Unmarshal(output.Bytes(), &result)
	if decodeErr != nil || (runErr != nil && !errors.Is(runErr, ctx.Err())) {
		return fmt.Errorf("%w: mount worker for %s: %w: %s", ErrMountCleanup, request.Rootfs, errors.Join(runErr, decodeErr), stderr.String())
	}
	if len(result.RootEntries) > 0 {
		buildLogger.Info(ctx, "Root filesystem structure: "+strings.Join(result.RootEntries, ", "))
	}
	if result.Cleanup {
		return errors.Join(ctx.Err(), fmt.Errorf("%w: %s", ErrMountCleanup, result.Error))
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if result.TooLarge != nil {
		return result.TooLarge
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	return nil
}
