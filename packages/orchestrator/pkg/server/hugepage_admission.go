//go:build linux

package server

import (
	"context"
	"fmt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
)

type hugePageMetrics struct {
	HugePagesUsed, HugePagesReserved, HugePagesBase, HugePagesCapacity, HugePageSizeBytes uint64
	HugePagesBaseRead                                                                     bool
}

// Read current kernel counters; admission must not use a cached sample.
func readHugePageMetrics(context.Context) (*hugePageMetrics, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	values := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "HugePages_Total:", "HugePages_Free:", "HugePages_Rsvd:", "Hugepagesize:":
			n, e := strconv.ParseUint(fields[1], 10, 64)
			if e != nil {
				return nil, e
			}
			values[fields[0]] = n
		}
	}
	for _, key := range []string{"HugePages_Total:", "HugePages_Free:", "HugePages_Rsvd:", "Hugepagesize:"} {
		if _, ok := values[key]; !ok {
			return nil, fmt.Errorf("missing %s", key)
		}
	}
	raw, err := os.ReadFile("/proc/sys/vm/nr_hugepages")
	if err != nil {
		return nil, err
	}
	base, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return nil, err
	}
	total, free := values["HugePages_Total:"], values["HugePages_Free:"]
	if free > total {
		return nil, fmt.Errorf("invalid hugepage counters")
	}
	return &hugePageMetrics{HugePagesUsed: total - free, HugePagesReserved: values["HugePages_Rsvd:"], HugePagesBase: base, HugePagesCapacity: base, HugePageSizeBytes: values["Hugepagesize:"] * 1024, HugePagesBaseRead: true}, nil
}

type hugePageAdmission struct {
	// read serves the last good sample when a read fails, and errs only
	// before the first one.
	read func(context.Context) (*hugePageMetrics, error)

	mu         sync.Mutex
	pendingMiB int64
	budgetMiB  uint64
}

// hugePageShortage is a start the free pages cannot back.
type hugePageShortage struct {
	ramMB        int64
	neededPages  uint64
	freePages    uint64
	pendingPages uint64
}

// freeHugePages is how many hugepages the node can still allocate: the pool
// ceiling (base pool plus overcommit allowance) less the pages in use and the
// pages reserved by mappings not yet faulted. ok is false when the metrics
// carry no ceiling or no page size, as placement treats them.
func freeHugePages(m *hugePageMetrics) (uint64, bool) {
	if m == nil || m.HugePagesCapacity == 0 || m.HugePageSizeBytes == 0 {
		return 0, false
	}
	committed := m.HugePagesUsed + m.HugePagesReserved
	if committed >= m.HugePagesCapacity {
		return 0, true
	}

	return m.HugePagesCapacity - committed, true
}

func hugePagesFor(mib int64, pageSize uint64) uint64 {
	if mib <= 0 || pageSize == 0 {
		return 0
	}
	bytes := uint64(mib) << 20

	return (bytes + pageSize - 1) / pageSize
}

// admit backports the upstream in-flight gate. Strict mode rejects missing
// current counters and bounds every mapping by the persistent base pool.
func (a *hugePageAdmission) admit(ctx context.Context, ramMB int64) (func(), *hugePageShortage) {
	a.mu.Lock()
	defer a.mu.Unlock()

	m, err := a.read(ctx)
	free, ok := freeHugePages(m)
	if a.budgetMiB > 0 {
		if err != nil || m == nil || !m.HugePagesBaseRead || m.HugePageSizeBytes < 1<<20 || ramMB <= 0 || uint64(ramMB) > math.MaxUint64>>20 {
			return nil, &hugePageShortage{ramMB: ramMB}
		}
		// Kernel reservations cover full mapped guest RAM, including untouched
		// pages. Never count surplus or the kernel overcommit allowance.
		ceiling := min(m.HugePagesBase, a.budgetMiB/(m.HugePageSizeBytes>>20))
		committed := m.HugePagesUsed + m.HugePagesReserved
		free, ok = ceiling-min(ceiling, committed), true
	}
	if err == nil && ok {
		needed := hugePagesFor(ramMB, m.HugePageSizeBytes)
		pending := hugePagesFor(a.pendingMiB, m.HugePageSizeBytes)
		if pending > free || needed > free-pending {
			return nil, &hugePageShortage{ramMB: ramMB, neededPages: needed, freePages: free, pendingPages: pending}
		}
	}

	if a.budgetMiB > 0 {
		// Account each mapping's page rounding separately for mixed odd-MiB sizes.
		ramMB = int64(hugePagesFor(ramMB, m.HugePageSizeBytes) * (m.HugePageSizeBytes >> 20))
	}
	return a.holdLocked(ramMB), nil
}

// hold takes ramMB of hugepages for a start that is never refused until
// release is called.
func (a *hugePageAdmission) hold(ramMB int64) func() {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.holdLocked(ramMB)
}

func (a *hugePageAdmission) holdLocked(ramMB int64) func() {
	a.pendingMiB += ramMB
	var once sync.Once

	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.pendingMiB -= ramMB
			a.mu.Unlock()
		})
	}
}

func (s *Server) admitHugePages(ctx context.Context, ramMiB int64, hugePages bool) (func(), error) {
	if s.hugePages == nil {
		return func() {}, nil
	}
	if !hugePages {
		return nil, status.Error(codes.FailedPrecondition, "strict memory admission requires hugepage-backed guests")
	}
	release, shortage := s.hugePages.admit(ctx, ramMiB)
	if shortage != nil {
		return nil, status.Errorf(codes.ResourceExhausted, "hugepage memory capacity unavailable for %d MiB", ramMiB)
	}
	return release, nil
}
