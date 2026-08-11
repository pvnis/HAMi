/*
Copyright 2024 The HAMi Authors.

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

package amd

import (
	"testing"

	"github.com/Project-HAMi/HAMi/pkg/device"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	testMemRes = "amd.com/gpu-vram-mib"
	testCntRes = "amd.com/gpu"
)

func testDevices() *AMDDevices {
	return InitAMDGPUDevice(AMDConfig{
		ResourceCountName:  testCntRes,
		ResourceMemoryName: testMemRes,
		DefaultCUs:         64,
		CUGroupSize:        2,
	})
}

// sens1: a Navi 32 with 54 compute units advertising 23 slices of 512 MiB.
func testNode() corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "sens1",
			Labels: map[string]string{NodeCUsLabel: "54", NodeGPUCountLabel: "1"},
		},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceName(testMemRes): *resource.NewQuantity(23, resource.DecimalSI),
			},
		},
	}
}

func ctrRequesting(slices int64) corev1.Container {
	return corev1.Container{
		Name: "c",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceName(testMemRes): *resource.NewQuantity(slices, resource.DecimalSI),
			},
		},
	}
}

func Test_MutateAdmission(t *testing.T) {
	dev := testDevices()
	pod := &corev1.Pod{}

	ctr := ctrRequesting(4)
	if ok, err := dev.MutateAdmission(&ctr, pod); err != nil || !ok {
		t.Errorf("a pod requesting %s should be claimed: ok=%v err=%v", testMemRes, ok, err)
	}

	plain := corev1.Container{Name: "c"}
	if ok, _ := dev.MutateAdmission(&plain, pod); ok {
		t.Error("a pod requesting no GPU should not be claimed")
	}
}

func Test_GetNodeDevices(t *testing.T) {
	dev := testDevices()

	got, err := dev.GetNodeDevices(testNode())
	if err != nil {
		t.Fatal(err)
	}
	// One card, not one per slice: reading the slice count as a device count is
	// what upstream did and it would allocate a GPU per 512 MiB.
	if len(got) != 1 {
		t.Fatalf("got %d devices, want 1", len(got))
	}
	if got[0].Devmem != 23 {
		t.Errorf("Devmem = %d, want 23 slices", got[0].Devmem)
	}
	if got[0].Devcore != 54 {
		t.Errorf("Devcore = %d, want the node label's 54 CUs", got[0].Devcore)
	}

	// Without the label the configured default stands in.
	n := testNode()
	delete(n.Labels, NodeCUsLabel)
	got, err = dev.GetNodeDevices(n)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Devcore != 64 {
		t.Errorf("Devcore = %d, want the configured default 64", got[0].Devcore)
	}

	// A node advertising no AMD memory has no AMD GPU.
	bare := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "sensai"}}
	if _, err := dev.GetNodeDevices(bare); err == nil {
		t.Error("a node with no AMD resource should report an error")
	}
}

func Test_GenerateResourceRequests(t *testing.T) {
	dev := testDevices()

	ctr := ctrRequesting(4)
	req := dev.GenerateResourceRequests(&ctr)
	if req.Nums != 1 {
		t.Errorf("Nums = %d, want 1: the units are slices of one card, not cards", req.Nums)
	}
	if req.Memreq != 4 {
		t.Errorf("Memreq = %d, want 4 slices", req.Memreq)
	}

	plain := corev1.Container{Name: "c"}
	if got := dev.GenerateResourceRequests(&plain); got.Nums != 0 {
		t.Errorf("a container requesting nothing produced %+v", got)
	}
}

func Test_checkType(t *testing.T) {
	dev := testDevices()
	if _, ok, _ := dev.checkType(device.ContainerDeviceRequest{Type: AMDDevice}); !ok {
		t.Error("AMDDevice should be accepted")
	}
	if _, ok, _ := dev.checkType(device.ContainerDeviceRequest{Type: "NVIDIA"}); ok {
		t.Error("a foreign device type should be rejected")
	}
}

func usage(totalSlices, totalCUs int32, pods ...*corev1.Pod) *device.DeviceUsage {
	u := &device.DeviceUsage{
		ID:        "sens1-AMDGPU-0",
		Index:     0,
		Count:     totalSlices,
		Totalmem:  totalSlices,
		Totalcore: totalCUs,
		Type:      AMDDevice,
		Health:    true,
	}
	for _, p := range pods {
		u.PodInfos = append(u.PodInfos, &device.PodInfo{Pod: p})
	}
	return u
}

func podWithUID(uid string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "placed-" + uid, UID: types.UID(uid)}}
}

// nodeWithAllocs builds the node record that Fit reads occupancy from.
func nodeWithAllocs(allocs map[string]string) *device.NodeInfo {
	m := map[string]cuAllocation{}
	for uid, mask := range allocs {
		m[uid] = cuAllocation{Device: "sens1-AMDGPU-0", Mask: mask}
	}
	enc, _ := formatAllocations(m)
	return &device.NodeInfo{ID: "sens1", Node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "sens1", Annotations: map[string]string{CUAllocAnnotation: enc}}}}
}

// nodeWithRawAllocs allows a deliberately corrupt record.
func nodeWithRawAllocs(raw string) *device.NodeInfo {
	return &device.NodeInfo{ID: "sens1", Node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "sens1", Annotations: map[string]string{CUAllocAnnotation: raw}}}}
}

func maskFrom(t *testing.T, devs map[string]device.ContainerDevices) string {
	t.Helper()
	for _, cds := range devs {
		for _, cd := range cds {
			if m, ok := cd.CustomInfo[cuMaskKey].(string); ok {
				return m
			}
		}
	}
	t.Fatal("no CU mask in the allocation")
	return ""
}

func TestDevices_Fit(t *testing.T) {
	dev := testDevices()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "gpu-test"}}
	req := device.ContainerDeviceRequest{Nums: 1, Type: AMDDevice, Memreq: 4}

	// An empty device: the pod gets the lowest run. 4 of 23 slices of 54 CUs
	// is 9, rounded down to the 8 that fit in whole pairs.
	fit, devs, reason := dev.Fit([]*device.DeviceUsage{usage(23, 54)}, req, pod, nil, nil)
	if !fit {
		t.Fatalf("expected a fit, got %q", reason)
	}
	if got := maskFrom(t, devs); got != "0xff" {
		t.Errorf("mask = %s, want 0xff", got)
	}

	// With a neighbour already holding the low 8, the next pod must be placed
	// above it and must not overlap.
	fit, devs, reason = dev.Fit([]*device.DeviceUsage{usage(23, 54, podWithUID("u1"))}, req, pod,
		nodeWithAllocs(map[string]string{"u1": "0xff"}), nil)
	if !fit {
		t.Fatalf("expected a fit, got %q", reason)
	}
	if got := maskFrom(t, devs); got != "0xff00" {
		t.Errorf("mask = %s, want 0xff00 (disjoint from the neighbour)", got)
	}
}

func TestFitRefusesWhenComputeIsExhausted(t *testing.T) {
	dev := testDevices()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	req := device.ContainerDeviceRequest{Nums: 1, Type: AMDDevice, Memreq: 12}

	// Every compute unit is spoken for. Placing the pod anyway would hand it
	// units another sandbox is already using, so it must be refused.
	full := "0x3fffffffffffff" // 54 CUs
	fit, _, reason := dev.Fit([]*device.DeviceUsage{usage(23, 54, podWithUID("u1"))}, req, pod,
		nodeWithAllocs(map[string]string{"u1": full}), nil)
	if fit {
		t.Error("placed a pod on a device with no free compute units")
	}
	if reason == "" {
		t.Error("expected a reason for the refusal")
	}
}

func TestFitRespectsMemory(t *testing.T) {
	dev := testDevices()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}

	u := usage(23, 54)
	u.Usedmem = 20
	req := device.ContainerDeviceRequest{Nums: 1, Type: AMDDevice, Memreq: 4}
	if fit, _, _ := dev.Fit([]*device.DeviceUsage{u}, req, pod, nil, nil); fit {
		t.Error("placed a 4-slice request on a device with 3 slices free")
	}
}

// An unreadable mask on a placed pod must make the device look full rather
// than free: overlapping compute units break the partition silently, whereas
// refusing to place is visible and recoverable.
func TestFitTreatsUnreadableMasksAsOccupied(t *testing.T) {
	dev := testDevices()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	req := device.ContainerDeviceRequest{Nums: 1, Type: AMDDevice, Memreq: 4}

	fit, _, _ := dev.Fit([]*device.DeviceUsage{usage(23, 54, podWithUID("u1"))}, req, pod,
		nodeWithAllocs(map[string]string{"u1": "not-a-mask"}), nil)
	if fit {
		t.Error("placed a pod despite being unable to tell which compute units were free")
	}
}

func Test_PatchAnnotations(t *testing.T) {
	dev := testDevices()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	anns := map[string]string{}

	pd := device.PodDevices{
		AMDDevice: device.PodSingleDevice{
			device.ContainerDevices{{
				Idx:        0,
				UUID:       "sens1-AMDGPU-0",
				Type:       AMDDevice,
				Usedmem:    4,
				Usedcores:  8,
				CustomInfo: map[string]any{cuMaskKey: "0xff"},
			}},
		},
	}
	// The node record cannot be written without a cluster; that failure is
	// logged and non-fatal, and the pod annotations must still be correct.
	got := dev.PatchAnnotations(pod, &anns, pd)

	// The mask has to be written as its own annotation: CustomInfo does not
	// survive EncodeContainerDevices, and this annotation is both what
	// amdproxy reads and how the next scheduling cycle learns what is taken.
	if got[CUMaskAnnotation] != "0xff" {
		t.Errorf("%s = %q, want 0xff", CUMaskAnnotation, got[CUMaskAnnotation])
	}
	if got[device.SupportDevices[AMDDevice]] == "" {
		t.Error("HAMi's own device annotation should still be written")
	}
}

// A pod placed moments earlier in the same scheduling pass has no CU-mask
// annotation yet — the patch has not been written. Its allocation is only
// visible in what HAMi recorded at bind, so occupancy must be read from there
// too. Reading the annotation alone hands two pods submitted together the same
// compute units, which is exactly what happened on hardware before this.
func TestFitSeesASiblingPlacedInTheSamePass(t *testing.T) {
	// Two pods submitted together: the second is filtered before the first's
	// node record is visible - measured at 8ms apart on hardware. The
	// in-process overlay is what stops them both being handed the lowest run.
	dev := testDevices()
	req := device.ContainerDeviceRequest{Nums: 1, Type: AMDDevice, Memreq: 4}
	empty := &device.NodeInfo{ID: "sens1", Node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "sens1"}}}

	first := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "first", UID: types.UID("u1")}}
	_, devs1, _ := dev.Fit([]*device.DeviceUsage{usage(23, 54)}, req, first, empty, nil)
	if got := maskFrom(t, devs1); got != "0xff" {
		t.Fatalf("first mask = %s, want 0xff", got)
	}

	// Nothing has been written to the node yet, and the second pod's own
	// DeviceUsage still shows an empty device.
	second := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "second", UID: types.UID("u2")}}
	fit, devs2, reason := dev.Fit([]*device.DeviceUsage{usage(23, 54)}, req, second, empty, nil)
	if !fit {
		t.Fatalf("expected a fit, got %q", reason)
	}
	if got := maskFrom(t, devs2); got != "0xff00" {
		t.Errorf("second mask = %s, want 0xff00: must not reuse the sibling's run", got)
	}
}

// A pod that has gone releases its compute units without anything having to
// clean up after it: its record is dropped because it is no longer among the
// device's PodInfos.
func TestDepartedPodsReleaseTheirCUs(t *testing.T) {
	dev := testDevices()
	req := device.ContainerDeviceRequest{Nums: 1, Type: AMDDevice, Memreq: 4}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "new", UID: types.UID("u9")}}

	// The node still records u1 and u2, but only u2 is still placed.
	node := nodeWithAllocs(map[string]string{"u1": "0xff", "u2": "0xff00"})
	devs := []*device.DeviceUsage{usage(23, 54, podWithUID("u2"))}

	fit, got, reason := dev.Fit(devs, req, pod, node, nil)
	if !fit {
		t.Fatalf("expected a fit, got %q", reason)
	}
	if m := maskFrom(t, got); m != "0xff" {
		t.Errorf("mask = %s, want 0xff: u1 has gone so its run is free again", m)
	}
}

// The node record is the durable channel now, so what has to survive is the
// annotation round trip of that record, not of the device encoding.
func TestNodeRecordRoundTrip(t *testing.T) {
	in := map[string]cuAllocation{
		"u1": {Device: "sens1-AMDGPU-0", Mask: "0xff"},
		"u2": {Device: "sens1-AMDGPU-0", Mask: "0xff00"},
	}
	enc, err := formatAllocations(in)
	if err != nil {
		t.Fatal(err)
	}
	out := parseAllocations(enc)
	if len(out) != 2 || out["u1"].Mask != "0xff" || out["u2"].Device != "sens1-AMDGPU-0" {
		t.Fatalf("round trip lost data: %v", out)
	}

	occ, ok := occupiedFrom(out, "sens1-AMDGPU-0", 54)
	if !ok {
		t.Fatal("occupancy unreadable")
	}
	if occ.String() != "0xffff" {
		t.Errorf("occupied = %s, want 0xffff", occ.String())
	}

	// A record for a different device must not count against this one.
	out["u3"] = cuAllocation{Device: "sens1-AMDGPU-1", Mask: "0x3f"}
	occ, _ = occupiedFrom(out, "sens1-AMDGPU-0", 54)
	if occ.String() != "0xffff" {
		t.Errorf("occupied = %s after adding another device's record, want 0xffff", occ.String())
	}

	// Garbage must report unreadable rather than empty.
	if _, ok := occupiedFrom(map[string]cuAllocation{"u": {Device: "sens1-AMDGPU-0", Mask: "nope"}}, "sens1-AMDGPU-0", 54); ok {
		t.Error("an unparseable mask should report the device as unreadable")
	}
	if got := parseAllocations("{not json"); len(got) != 0 {
		t.Errorf("corrupt annotation should parse to nothing, got %v", got)
	}
}

// The AMD device plugin publishes the card's real shape; that must win over
// the node labels and the configured default, which are only a fallback.
func TestGetNodeDevicesPrefersThePluginRegistration(t *testing.T) {
	dev := testDevices()
	n := testNode()
	n.Labels[NodeCUsLabel] = "999" // deliberately wrong
	n.Annotations = map[string]string{
		RegisterAnnotation: `[{"id":"sens1-AMDGPU-1","count":23,"devmem":23,"devcore":54,` +
			`"cugroup":2,"type":"ip discovery","health":true,"nodeindex":1}]`,
	}

	got, err := dev.GetNodeDevices(n)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d devices, want 1", len(got))
	}
	if got[0].Devcore != 54 {
		t.Errorf("Devcore = %d, want the plugin's 54, not the label's 999", got[0].Devcore)
	}
	if got[0].ID != "sens1-AMDGPU-1" {
		t.Errorf("ID = %q, want the plugin's id", got[0].ID)
	}
	if g, ok := got[0].CustomInfo[cuGroupKey].(int); !ok || g != 2 {
		t.Errorf("cuGroup = %v, want 2 carried from the registration", got[0].CustomInfo[cuGroupKey])
	}

	// A malformed registration falls back rather than failing the node.
	n.Annotations[RegisterAnnotation] = "{garbage"
	got, err = dev.GetNodeDevices(n)
	if err != nil || len(got) != 1 || got[0].Devcore != 999 {
		t.Errorf("expected fallback to the label, got %v err=%v", got, err)
	}
}
