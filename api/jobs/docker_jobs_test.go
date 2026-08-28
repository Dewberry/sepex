package jobs

import "testing"

func TestGPUDeviceRequestsEmptyWhenNoDevices(t *testing.T) {
	if got := gpuDeviceRequests(nil); got != nil {
		t.Errorf("gpuDeviceRequests(nil) = %v, want nil so Docker is asked for no device", got)
	}
	if got := gpuDeviceRequests([]GPUDevice{}); got != nil {
		t.Errorf("gpuDeviceRequests(empty) = %v, want nil", got)
	}
}

func TestGPUDeviceRequestsNamesExactDevices(t *testing.T) {
	devices := []GPUDevice{{Index: 1, UUID: "GPU-aaa"}, {Index: 3, UUID: "GPU-ccc"}}

	reqs := gpuDeviceRequests(devices)
	if len(reqs) != 1 {
		t.Fatalf("got %d device requests, want exactly 1", len(reqs))
	}
	req := reqs[0]

	if req.Driver != "nvidia" {
		t.Errorf("Driver = %q, want nvidia", req.Driver)
	}
	// Count must stay zero: setting both Count and DeviceIDs is rejected by
	// Docker, and Count would let it pick devices the pool did not allocate.
	if req.Count != 0 {
		t.Errorf("Count = %d, want 0 when devices are named explicitly", req.Count)
	}
	if len(req.Capabilities) != 1 || len(req.Capabilities[0]) != 1 || req.Capabilities[0][0] != "gpu" {
		t.Errorf("Capabilities = %v, want [[gpu]]", req.Capabilities)
	}
	want := []string{"GPU-aaa", "GPU-ccc"}
	if len(req.DeviceIDs) != len(want) {
		t.Fatalf("DeviceIDs = %v, want %v", req.DeviceIDs, want)
	}
	for i := range want {
		if req.DeviceIDs[i] != want[i] {
			t.Errorf("DeviceIDs[%d] = %q, want %q", i, req.DeviceIDs[i], want[i])
		}
	}
}

func TestGPUDeviceRequestsFallsBackToIndex(t *testing.T) {
	// Devices carry no UUID when GPU verification was skipped.
	reqs := gpuDeviceRequests([]GPUDevice{{Index: 2}})
	if got := reqs[0].DeviceIDs[0]; got != "2" {
		t.Errorf("DeviceIDs[0] = %q, want the index %q", got, "2")
	}
}
