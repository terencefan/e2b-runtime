//go:build linux

package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testHugePageSize = 2 << 20

// fakeMeminfo serves fixed hugepage counters and counts the reads.
type fakeMeminfo struct {
	memory hugePageMetrics
	err    error
	reads  atomic.Int64
}

func (f *fakeMeminfo) read(context.Context) (*hugePageMetrics, error) {
	f.reads.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	m := f.memory

	return &m, nil
}

// pool is a node with capacity pages of which used are faulted and reserved
// are reserved but not yet faulted.
func pool(capacity, used, reserved uint64) *fakeMeminfo {
	return &fakeMeminfo{memory: hugePageMetrics{

		HugePagesUsed:     used,
		HugePagesReserved: reserved,
		HugePageSizeBytes: testHugePageSize,
		HugePagesCapacity: capacity,
	}}
}

func TestStrictHugePageAdmissionMixedSizes(t *testing.T) {
	memory := pool(16384, 0, 0) // 32 GiB ceiling, intentionally larger than budget.
	memory.memory.HugePagesBaseRead = true
	memory.memory.HugePagesBase = 8192 // 16 GiB base pool.
	admission := &hugePageAdmission{read: memory.read, budgetMiB: 12 * 1024}
	first, shortage := admission.admit(t.Context(), 2*1024)
	require.Nil(t, shortage)
	second, shortage := admission.admit(t.Context(), 8*1024)
	require.Nil(t, shortage)
	_, shortage = admission.admit(t.Context(), 4*1024)
	require.NotNil(t, shortage)
	last, shortage := admission.admit(t.Context(), 2*1024)
	require.Nil(t, shortage)
	first()
	first()
	second()
	last()
	assert.Zero(t, admission.pendingMiB)
	// Once mappings commit RAM, kernel reservations carry the full amount.
	memory.memory.HugePagesReserved = 6144
	_, shortage = admission.admit(t.Context(), 1)
	require.NotNil(t, shortage)
}

func TestStrictHugePageAdmissionFailsClosed(t *testing.T) {
	for _, memory := range []*fakeMeminfo{pool(0, 0, 0), {err: errors.New("read failed")}, pool(8192, 0, 0)} {
		admission := &hugePageAdmission{read: memory.read, budgetMiB: 4096}
		release, shortage := admission.admit(t.Context(), 1024)
		require.NotNil(t, shortage)
		require.Nil(t, release)
	}
}

func TestStrictHugePageAdmissionBasePoolOnly(t *testing.T) {
	memory := pool(16384, 0, 0)
	memory.memory.HugePagesBaseRead = true
	memory.memory.HugePagesBase = 2048
	admission := &hugePageAdmission{read: memory.read, budgetMiB: 8192}
	_, shortage := admission.admit(t.Context(), 4097)
	require.NotNil(t, shortage, "overcommit ceiling cannot enlarge the base pool")
}

func TestStrictHugePageAdmissionConcurrentMixedSizes(t *testing.T) {
	memory := pool(8192, 0, 0)
	memory.memory.HugePagesBaseRead = true
	memory.memory.HugePagesBase = 8192
	a := &hugePageAdmission{read: memory.read, budgetMiB: 16384}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var releases []func()
	for i := range 200 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			release, shortage := a.admit(t.Context(), int64([]int{511, 1024, 2048, 4096}[i%4]))
			if shortage != nil {
				return
			}
			mu.Lock()
			releases = append(releases, release)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	require.LessOrEqual(t, a.pendingMiB, int64(16384))
	require.Greater(t, a.pendingMiB, int64(16384-512))
	for _, release := range releases {
		release()
		release()
	}
	require.Zero(t, a.pendingMiB)
}
