package jobs

import (
	"fmt"
	"sync"
	"testing"
)

func testDevices(n int) []GPUDevice {
	devices := make([]GPUDevice, n)
	for i := range devices {
		devices[i] = GPUDevice{Index: i, UUID: fmt.Sprintf("GPU-%d", i)}
	}
	return devices
}

func TestDeviceIDPrefersUUID(t *testing.T) {
	if got := (GPUDevice{Index: 3, UUID: "GPU-abc"}).DeviceID(); got != "GPU-abc" {
		t.Errorf("DeviceID() = %q, want the UUID", got)
	}
	// Falls back to the index when verification was skipped.
	if got := (GPUDevice{Index: 3}).DeviceID(); got != "3" {
		t.Errorf("DeviceID() = %q, want %q", got, "3")
	}
}

func TestNewResourcePoolCopiesAndSortsDevices(t *testing.T) {
	devices := []GPUDevice{{Index: 2}, {Index: 0}, {Index: 1}}
	rp := NewResourcePool(8, 8192, devices)

	// Mutating the caller's slice must not affect the pool.
	devices[0] = GPUDevice{Index: 99}

	status := rp.GetStatus()
	if status.MaxGPUs != 3 {
		t.Fatalf("MaxGPUs = %d, want 3", status.MaxGPUs)
	}
	for i, g := range status.GPUs {
		if g.Index != i {
			t.Errorf("GPUs[%d].Index = %d, want %d (devices should be index-ordered)", i, g.Index, i)
		}
	}
}

func TestTryReserveAssignsDistinctDevices(t *testing.T) {
	rp := NewResourcePool(8, 8192, testDevices(2))

	first, ok := rp.TryReserve("job-a", 1, 100, 1)
	if !ok || len(first) != 1 {
		t.Fatalf("first reserve: got %v, ok=%v; want one device", first, ok)
	}
	second, ok := rp.TryReserve("job-b", 1, 100, 1)
	if !ok || len(second) != 1 {
		t.Fatalf("second reserve: got %v, ok=%v; want one device", second, ok)
	}
	if first[0].Index == second[0].Index {
		t.Fatalf("both jobs were given GPU %d", first[0].Index)
	}

	// Pool is now exhausted even though CPU and memory remain.
	if _, ok := rp.TryReserve("job-c", 1, 100, 1); ok {
		t.Error("third reserve succeeded on a 2-GPU pool")
	}
}

func TestTryReserveIsAtomicAcrossResources(t *testing.T) {
	rp := NewResourcePool(1, 8192, testDevices(4))

	// Fails on CPU. GPUs must not be consumed as a side effect.
	if _, ok := rp.TryReserve("job-a", 2, 100, 2); ok {
		t.Fatal("reserve succeeded despite exceeding maxCPUs")
	}
	if used := rp.GetStatus().UsedGPUs; used != 0 {
		t.Errorf("UsedGPUs = %d after a failed reserve, want 0", used)
	}

	// Fails on GPU. CPU and memory must not be consumed either.
	if _, ok := rp.TryReserve("job-b", 1, 100, 9); ok {
		t.Fatal("reserve succeeded despite requesting more GPUs than exist")
	}
	status := rp.GetStatus()
	if status.UsedCPUs != 0 || status.UsedMemory != 0 {
		t.Errorf("UsedCPUs=%.2f UsedMemory=%d after a failed reserve, want 0 and 0", status.UsedCPUs, status.UsedMemory)
	}
}

func TestReleaseReturnsExactDevices(t *testing.T) {
	rp := NewResourcePool(8, 8192, testDevices(3))

	held, _ := rp.TryReserve("job-a", 1, 100, 2)
	rp.Release(1, 100, held[:1])

	status := rp.GetStatus()
	if status.UsedGPUs != 1 {
		t.Fatalf("UsedGPUs = %d after releasing one of two devices, want 1", status.UsedGPUs)
	}
	// The still-held device must remain attributed to its job.
	for _, g := range status.GPUs {
		if g.Index == held[1].Index && g.JobID != "job-a" {
			t.Errorf("GPU %d holder = %q, want job-a", g.Index, g.JobID)
		}
		if g.Index == held[0].Index && g.JobID != "" {
			t.Errorf("GPU %d holder = %q, want it freed", g.Index, g.JobID)
		}
	}
}

func TestDoubleReleaseDoesNotDuplicateDevice(t *testing.T) {
	rp := NewResourcePool(8, 8192, testDevices(1))

	held, _ := rp.TryReserve("job-a", 1, 100, 1)
	rp.Release(1, 100, held)
	rp.Release(1, 100, held) // duplicate, must be ignored

	// A duplicated device would let two jobs reserve the same GPU.
	if _, ok := rp.TryReserve("job-b", 1, 100, 1); !ok {
		t.Fatal("could not reserve the released device")
	}
	if _, ok := rp.TryReserve("job-c", 1, 100, 1); ok {
		t.Fatal("a second job reserved the same GPU; the double release duplicated it")
	}
}

func TestReserveForceReclaimsNamedDevices(t *testing.T) {
	rp := NewResourcePool(8, 8192, testDevices(3))

	// A surviving container holds GPU 1 specifically, not "one GPU".
	rp.ReserveForce("recovered", 1, 100, []GPUDevice{{Index: 1, UUID: "GPU-1"}})

	assigned, ok := rp.TryReserve("job-a", 1, 100, 2)
	if !ok {
		t.Fatal("could not reserve the two remaining GPUs")
	}
	for _, d := range assigned {
		if d.Index == 1 {
			t.Fatal("GPU 1 was handed out while still held by the recovered job")
		}
	}
}

func TestReserveForceIgnoresUnknownAndHeldDevices(t *testing.T) {
	rp := NewResourcePool(8, 8192, testDevices(2))

	held, _ := rp.TryReserve("job-a", 1, 100, 1)
	// Index 7 is not in the pool; held[0] already belongs to job-a.
	rp.ReserveForce("recovered", 0, 0, []GPUDevice{{Index: 7}, held[0]})

	status := rp.GetStatus()
	if status.UsedGPUs != 1 {
		t.Errorf("UsedGPUs = %d, want 1", status.UsedGPUs)
	}
	for _, g := range status.GPUs {
		if g.Index == held[0].Index && g.JobID != "job-a" {
			t.Errorf("GPU %d holder = %q, want the original job-a", g.Index, g.JobID)
		}
	}
}

func TestQueuedCountsClampAtZero(t *testing.T) {
	rp := NewResourcePool(8, 8192, testDevices(2))

	rp.AddQueued(1, 100, 1)
	rp.RemoveQueued(2, 200, 2) // over-removes

	status := rp.GetStatus()
	if status.QueuedCPUs != 0 || status.QueuedMemory != 0 || status.QueuedGPUs != 0 {
		t.Errorf("queued counts = (%.2f, %d, %d), want all zero",
			status.QueuedCPUs, status.QueuedMemory, status.QueuedGPUs)
	}
}

func TestConcurrentReserveNeverDoubleAllocates(t *testing.T) {
	const devices = 4
	rp := NewResourcePool(1000, 1000000, testDevices(devices))

	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[int]string{}

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			jobID := fmt.Sprintf("job-%d", i)
			assigned, ok := rp.TryReserve(jobID, 1, 1, 1)
			if !ok {
				return
			}
			mu.Lock()
			if prev, dup := seen[assigned[0].Index]; dup {
				t.Errorf("GPU %d handed to both %s and %s", assigned[0].Index, prev, jobID)
			}
			seen[assigned[0].Index] = jobID
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if len(seen) != devices {
		t.Errorf("allocated %d distinct GPUs, want %d", len(seen), devices)
	}
	if used := rp.GetStatus().UsedGPUs; used != devices {
		t.Errorf("UsedGPUs = %d, want %d", used, devices)
	}
}

func TestLookupGPUsMatchesEitherIdentifierForm(t *testing.T) {
	rp := NewResourcePool(8, 8192, testDevices(3))

	// Containers started while verification was on carry UUIDs.
	found, unresolved := rp.LookupGPUs([]string{"GPU-2", "GPU-0"})
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %v, want none", unresolved)
	}
	if len(found) != 2 || found[0].Index != 2 || found[1].Index != 0 {
		t.Errorf("found = %v, want devices 2 and 0 in that order", found)
	}

	// Containers started while verification was skipped carry bare indices.
	found, unresolved = rp.LookupGPUs([]string{"1"})
	if len(unresolved) != 0 || len(found) != 1 || found[0].Index != 1 {
		t.Errorf("found=%v unresolved=%v, want device 1 and nothing unresolved", found, unresolved)
	}
}

func TestLookupGPUsReportsUnknownIdentifiers(t *testing.T) {
	rp := NewResourcePool(8, 8192, testDevices(2))

	found, unresolved := rp.LookupGPUs([]string{"GPU-0", "GPU-from-another-host", "7"})
	if len(found) != 1 || found[0].Index != 0 {
		t.Errorf("found = %v, want only device 0", found)
	}
	if len(unresolved) != 2 {
		t.Errorf("unresolved = %v, want both unknown identifiers", unresolved)
	}
}

func TestReclaimContainerGPUsReturnsExactDevices(t *testing.T) {
	rp := NewResourcePool(8, 8192, testDevices(4))

	// A survivor holds GPU 2 specifically, not "one GPU".
	reclaimed := reclaimContainerGPUs(rp, "survivor", []string{"GPU-2"})
	if len(reclaimed) != 1 || reclaimed[0].Index != 2 {
		t.Fatalf("reclaimed = %v, want exactly device 2", reclaimed)
	}

	rp.ReserveForce("survivor", 1, 100, reclaimed)
	assigned, ok := rp.TryReserve("newcomer", 1, 100, 3)
	if !ok {
		t.Fatal("could not reserve the three remaining GPUs")
	}
	for _, d := range assigned {
		if d.Index == 2 {
			t.Fatal("GPU 2 was handed out while the recovered container still held it")
		}
	}
}

func TestReclaimContainerGPUsIgnoresUnrecognisedDevices(t *testing.T) {
	rp := NewResourcePool(8, 8192, testDevices(2))

	// An identifier this pool cannot name is reported and skipped rather than
	// guessed at, so the pool stays fully available.
	reclaimed := reclaimContainerGPUs(rp, "survivor", []string{"GPU-from-another-host"})
	if len(reclaimed) != 0 {
		t.Fatalf("reclaimed = %v, want nothing for an unrecognised device", reclaimed)
	}

	rp.ReserveForce("survivor", 0, 0, reclaimed)
	if _, ok := rp.TryReserve("newcomer", 1, 100, 2); !ok {
		t.Error("the pool was withheld even though nothing recognisable was reclaimed")
	}
}

func TestReclaimContainerGPUsKeepsRecognisedDevicesWhenMixed(t *testing.T) {
	rp := NewResourcePool(8, 8192, testDevices(3))

	// One identifier resolves, one does not. The resolvable device must still
	// be protected rather than discarded along with the unknown one.
	reclaimed := reclaimContainerGPUs(rp, "survivor", []string{"GPU-1", "GPU-from-another-host"})
	if len(reclaimed) != 1 || reclaimed[0].Index != 1 {
		t.Fatalf("reclaimed = %v, want exactly device 1", reclaimed)
	}

	rp.ReserveForce("survivor", 0, 0, reclaimed)
	assigned, ok := rp.TryReserve("newcomer", 1, 100, 2)
	if !ok {
		t.Fatal("could not reserve the two devices that remain free")
	}
	for _, d := range assigned {
		if d.Index == 1 {
			t.Error("GPU 1 was handed out while the recovered container held it")
		}
	}
}

func TestReclaimContainerGPUsNoDevices(t *testing.T) {
	rp := NewResourcePool(8, 8192, testDevices(2))
	if got := reclaimContainerGPUs(rp, "cpu-only-job", nil); got != nil {
		t.Errorf("reclaimed %v for a job with no GPUs, want nil", got)
	}
}
