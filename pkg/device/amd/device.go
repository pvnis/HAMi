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
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/device/common"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

type AMDDevices struct {
	resourceCountName  string
	resourceMemoryName string
	defaultCUs         int
	cuGroupSize        int
}

const (
	AMDDevice          = "AMDGPU"
	AMDCommonWord      = "AMDGPU"
	AMDDeviceSelection = "amd.com/gpu-index"
	AMDUseUUID         = "amd.com/use-gpu-uuid"
	AMDNoUseUUID       = "amd.com/nouse-gpu-uuid"
	AMDAssignedNode    = "amd.com/predicate-node"

	// CUMaskAnnotation is the flag amdproxy reads. The scheduler both writes
	// it and reads it back off already-placed pods to work out which compute
	// units on a device are spoken for, so the allocation state lives on the
	// pods themselves and survives a scheduler restart.
	CUMaskAnnotation = "dev.gvisor.flag.amdproxy-cu-mask"

	// NodeCUsLabel and NodeGPUCountLabel describe the node's AMD hardware.
	// They are labels rather than advertised resources because they are
	// properties of the card, not things a pod consumes.
	NodeCUsLabel      = "amd.com/gpu-cus"
	NodeGPUCountLabel = "amd.com/gpu-count"
)

type AMDConfig struct {
	ResourceCountName  string `yaml:"resourceCountName"`
	ResourceMemoryName string `yaml:"resourceMemoryName"`
	// DefaultCUs is used when a node carries no amd.com/gpu-cus label.
	DefaultCUs int `yaml:"defaultCUs"`
	// CUGroupSize is how many compute units must be enabled together. RDNA
	// pairs them into workgroup processors and KFD refuses a mask that splits
	// a pair, so this is 2 there; GCN and CDNA do not pair and use 1.
	CUGroupSize int `yaml:"cuGroupSize"`
}

func InitAMDGPUDevice(config AMDConfig) *AMDDevices {
	_, ok := device.SupportDevices[AMDDevice]
	if !ok {
		device.SupportDevices[AMDDevice] = "hami.io/amd-devices-allocated"
	}
	cus := config.DefaultCUs
	if cus <= 0 {
		cus = 64
	}
	group := config.CUGroupSize
	if group <= 0 {
		group = 2
	}
	return &AMDDevices{
		resourceCountName:  config.ResourceCountName,
		resourceMemoryName: config.ResourceMemoryName,
		defaultCUs:         cus,
		cuGroupSize:        group,
	}
}

func (dev *AMDDevices) CommonWord() string {
	return AMDCommonWord
}

func ParseConfig(fs *flag.FlagSet) {
}

func (dev *AMDDevices) MutateAdmission(ctr *corev1.Container, p *corev1.Pod) (bool, error) {
	_, ok := ctr.Resources.Limits[corev1.ResourceName(dev.resourceCountName)]
	if !ok {
		_, ok = ctr.Resources.Limits[corev1.ResourceName(dev.resourceMemoryName)]
	}
	klog.Infoln("MutateAdmsssion result", ok)
	return ok, nil
}

// nodeInt reads a non-negative integer from a node label, falling back to def.
func nodeInt(n corev1.Node, label string, def int) int {
	v, ok := n.Labels[label]
	if !ok {
		return def
	}
	i, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || i <= 0 {
		klog.ErrorS(err, "ignoring unusable node label", "node", n.Name, "label", label, "value", v)
		return def
	}
	return i
}

// GetNodeDevices reports the AMD GPUs on a node.
//
// Upstream fabricated one device per unit of the count resource, each with a
// hardcoded 192 GB of MI300X memory. Here the memory resource counts fixed-size
// VRAM slices of a *single* card, so the number of GPUs comes from a node label
// and the advertised capacity is divided between them. Compute units come from
// a label too, since the count differs per card (54 on Navi 32, 12 on a
// Phoenix APU) and nothing in the resource model carries it.
func (dev *AMDDevices) GetNodeDevices(n corev1.Node) ([]*device.DeviceInfo, error) {
	slices, ok := n.Status.Capacity.Name(corev1.ResourceName(dev.resourceMemoryName), resource.DecimalSI).AsInt64()
	if !ok || slices == 0 {
		return []*device.DeviceInfo{}, fmt.Errorf("device not found %s", dev.resourceMemoryName)
	}
	gpus := nodeInt(n, NodeGPUCountLabel, 1)
	cus := nodeInt(n, NodeCUsLabel, dev.defaultCUs)

	nodedevices := []*device.DeviceInfo{}
	for i := 0; i < gpus; i++ {
		nodedevices = append(nodedevices, &device.DeviceInfo{
			Index:        uint(i),
			ID:           n.Name + "-" + AMDDevice + "-" + fmt.Sprint(i),
			Count:        int32(slices),
			Devmem:       int32(slices / int64(gpus)),
			Devcore:      int32(cus),
			Type:         AMDDevice,
			Numa:         0,
			Health:       true,
			CustomInfo:   map[string]any{},
			DeviceVendor: AMDCommonWord,
		})
	}
	klog.V(4).InfoS("registered AMD node devices", "node", n.Name, "gpus", gpus, "slices", slices, "cus", cus)
	return nodedevices, nil
}

// PatchAnnotations records the allocation on the pod. Besides HAMi's own
// bookkeeping it writes the CU mask chosen in Fit, which is what actually
// enforces the compute partition: amdproxy applies it to every queue the
// sandbox creates and the GPU's command processor honours it, so the sandbox
// cannot widen it from inside.
//
// The annotation is also how the next scheduling cycle learns which compute
// units are taken — see Fit.
func (dev *AMDDevices) PatchAnnotations(pod *corev1.Pod, annoinput *map[string]string, pd device.PodDevices) map[string]string {
	devlist, ok := pd[AMDDevice]
	if ok && len(devlist) > 0 {
		(*annoinput)[device.SupportDevices[AMDDevice]] = device.EncodePodSingleDevice(devlist)
		for _, ctrdevs := range devlist {
			for _, cd := range ctrdevs {
				if mask, ok := cd.CustomInfo[cuMaskKey].(string); ok && mask != "" {
					// CustomInfo does not survive EncodeContainerDevices, so
					// the mask has to be written out as its own annotation.
					(*annoinput)[CUMaskAnnotation] = mask
					klog.InfoS("assigned AMD compute units", "pod", klog.KObj(pod), "device", cd.UUID, "cuMask", mask)
				}
			}
		}
	}
	klog.V(4).InfoS("annos", "input", (*annoinput))
	return *annoinput
}

func (dev *AMDDevices) LockNode(n *corev1.Node, p *corev1.Pod) error {
	return nil
}

func (dev *AMDDevices) ReleaseNodeLock(n *corev1.Node, p *corev1.Pod) error {
	return nil
}

func (dev *AMDDevices) NodeCleanUp(nn string) error {
	return nil
}

func (dev *AMDDevices) checkType(n device.ContainerDeviceRequest) (bool, bool, bool) {
	if strings.Compare(n.Type, AMDDevice) == 0 {
		return true, true, false
	}
	return false, false, false
}

func (dev *AMDDevices) CheckHealth(devType string, n *corev1.Node) (bool, bool) {
	return true, true
}

func (dev *AMDDevices) GetResourceNames() device.ResourceNames {
	return device.ResourceNames{
		ResourceCountName:  dev.resourceCountName,
		ResourceMemoryName: dev.resourceMemoryName,
		ResourceCoreName:   "",
	}
}

// GenerateResourceRequests reads what the container asked for.
//
// The AMD slice is one-dimensional: the pod asks for VRAM in fixed-size units
// and the compute it gets is proportional to that. Nums is always 1 because
// the units are slices of one card, not whole cards — reading them as a device
// count, as upstream does, would try to allocate one GPU per 512 MiB.
func (dev *AMDDevices) GenerateResourceRequests(ctr *corev1.Container) device.ContainerDeviceRequest {
	amdResourceMemory := corev1.ResourceName(dev.resourceMemoryName)
	v, ok := ctr.Resources.Limits[amdResourceMemory]
	if !ok {
		v, ok = ctr.Resources.Requests[amdResourceMemory]
	}
	if !ok {
		return device.ContainerDeviceRequest{}
	}
	n, ok := v.AsInt64()
	if !ok || n <= 0 {
		return device.ContainerDeviceRequest{}
	}
	klog.InfoS("detected AMD device request", "container", ctr.Name, "slices", n)
	return device.ContainerDeviceRequest{
		Nums:             1,
		Type:             AMDDevice,
		Memreq:           int32(n),
		MemPercentagereq: 0,
		// Filled in by Fit, which is the first place the device's compute unit
		// count is known.
		Coresreq: 0,
	}
}

func (dev *AMDDevices) ScoreNode(node *corev1.Node, podDevices device.PodSingleDevice, previous []*device.DeviceUsage, policy string) float32 {
	return 0
}

func (dev *AMDDevices) AddResourceUsage(pod *corev1.Pod, n *device.DeviceUsage, ctr *device.ContainerDevice) error {
	n.Used++
	n.Usedcores += ctr.Usedcores
	n.Usedmem += ctr.Usedmem
	return nil
}

// maskFromUUID pulls the CU mask out of a device UUID of the form
// "<device>[<mask>]". Returns "" if there is no suffix.
func maskFromUUID(uuid string) string {
	open := strings.Index(uuid, "[")
	if open < 0 || !strings.HasSuffix(uuid, "]") {
		return ""
	}
	return uuid[open+1 : len(uuid)-1]
}

// cuMaskKey is where Fit stashes the mask it chose so that PatchAnnotations
// can write it out. It never leaves the scheduler process under this name.
const cuMaskKey = "gvisorCUMask"

// occupiedCUs returns the compute units already handed to pods on a device.
//
// The state is read back off the pods rather than kept in the scheduler,
// because the scheduler can restart and a pod outlives it. A mask that cannot
// be parsed is treated as covering the whole device: refusing to place a pod is
// a great deal better than handing out compute units that overlap someone
// else's, which would silently break the partition rather than fail.
func (amddevice *AMDDevices) occupiedCUs(dev *device.DeviceUsage, total int) cuSet {
	var occupied cuSet
	full := func() cuSet {
		f := newCUSet(total)
		for i := 0; i < total; i++ {
			f.set(i)
		}
		return f
	}

	for _, pi := range dev.PodInfos {
		if pi == nil {
			continue
		}
		found := false

		// The allocation HAMi recorded when it placed the pod. This is the
		// only source that covers a pod placed moments ago in this same
		// scheduling pass, whose annotation patch may not have been written
		// yet — without it, two pods submitted together are both handed the
		// lowest free run and the partition is not a partition.
		for _, psd := range pi.Devices[AMDDevice] {
			for _, cd := range psd {
				raw := maskFromUUID(cd.UUID)
				if raw == "" {
					// Only set for an allocation made in this process; after a
					// restart HAMi rebuilds from the annotation and the UUID
					// suffix above is all that is left.
					if v, ok := cd.CustomInfo[cuMaskKey].(string); ok {
						raw = v
					}
				}
				if raw == "" {
					continue
				}
				mask, err := parseCUMask(raw)
				if err != nil {
					klog.ErrorS(err, "unreadable recorded CU mask; treating the device as fully occupied",
						"pod", klog.KObj(pi.Pod), "mask", raw)
					return full()
				}
				occupied.or(mask)
				found = true
			}
		}
		if found {
			continue
		}

		// MEASURED, 2026-08-11: this fallback can never fire. pi.Pod is the pod
		// as it arrived at the extender's filter request — phase Pending, with
		// only the annotations admission wrote. Neither this mask nor even
		// HAMi's own hami.io/amd-devices-allocated is on it, because both are
		// patched after the filter returns. Reading occupancy back off placed
		// pods is therefore not possible from here at all; the offsets need
		// storage of their own. Kept as a no-op so the finding is not retried.
		raw, ok := pi.Pod.Annotations[CUMaskAnnotation]
		if !ok || raw == "" {
			continue
		}
		mask, err := parseCUMask(raw)
		if err != nil {
			klog.ErrorS(err, "unreadable CU mask on a placed pod; treating the device as fully occupied",
				"pod", klog.KObj(pi.Pod), "mask", raw)
			return full()
		}
		occupied.or(mask)
	}
	return occupied
}

// Fit picks a device with room for the request and assigns it a disjoint,
// group-aligned run of compute units.
//
// Memory is still counted by kubelet through the extended resource, so this is
// not the only thing keeping a node from being oversubscribed; what only the
// scheduler can do is decide *which* compute units a pod gets, because that
// needs to know what the other pods on the node already hold.
func (amddevice *AMDDevices) Fit(devices []*device.DeviceUsage, request device.ContainerDeviceRequest, pod *corev1.Pod, nodeinfo *device.NodeInfo, allocated *device.PodDevices) (bool, map[string]device.ContainerDevices, string) {
	k := request
	klog.InfoS("allocating AMD device for container request", "pod", klog.KObj(pod), "request", k)
	tmpDevs := make(map[string]device.ContainerDevices)
	reason := make(map[string]int)

	for i := len(devices) - 1; i >= 0; i-- {
		dev := devices[i]
		if !strings.Contains(dev.Type, k.Type) {
			reason[common.CardTypeMismatch]++
			continue
		}
		if _, found, _ := amddevice.checkType(k); !found {
			reason[common.CardTypeMismatch]++
			continue
		}
		if !device.CheckUUID(pod.GetAnnotations(), dev.ID, AMDUseUUID, AMDNoUseUUID, amddevice.CommonWord()) {
			reason[common.CardUUIDMismatch]++
			continue
		}
		if dev.Totalmem > 0 && dev.Usedmem+k.Memreq > dev.Totalmem {
			reason[common.CardInsufficientMemory]++
			klog.V(5).InfoS(common.CardInsufficientMemory, "pod", klog.KObj(pod), "device", dev.ID,
				"used", dev.Usedmem, "request", k.Memreq, "total", dev.Totalmem)
			continue
		}

		total := int(dev.Totalcore)
		if total <= 0 {
			total = amddevice.defaultCUs
		}
		want := cusForRequest(int64(k.Memreq), int64(dev.Totalmem), total, amddevice.cuGroupSize)
		occ := amddevice.occupiedCUs(dev, total)
		klog.InfoS("AMD CU occupancy", "pod", klog.KObj(pod), "device", dev.ID,
			"podInfos", len(dev.PodInfos), "occupied", occ.String(), "occupiedCount", occ.count(),
			"usedcores", dev.Usedcores, "usedmem", dev.Usedmem)
		mask, ok := allocateCUs(occ, total, want, amddevice.cuGroupSize)
		if !ok {
			reason[common.CardInsufficientCore]++
			klog.V(5).InfoS(common.CardInsufficientCore, "pod", klog.KObj(pod), "device", dev.ID,
				"wantCUs", want, "totalCUs", total)
			continue
		}

		klog.InfoS("AMD device fits", "pod", klog.KObj(pod), "device", dev.ID,
			"slices", k.Memreq, "cus", want, "cuMask", mask.String())
		tmpDevs[k.Type] = append(tmpDevs[k.Type], device.ContainerDevice{
			Idx: int(dev.Index),
			// NOTE: the mask must NOT be smuggled in a "UUID[...]" suffix.
			// That was tried and it crash-loops the scheduler: a '[' in a
			// device UUID is HAMi's marker for a MIG instance, so the suffix
			// drives MIG code paths and panics with an index out of range.
			// Where the mask should durably live is still open — see the
			// README.
			UUID:      dev.ID,
			Type:      k.Type,
			Usedmem:   k.Memreq,
			Usedcores: int32(want),
			CustomInfo: map[string]any{
				cuMaskKey: mask.String(),
			},
		})
		return true, tmpDevs, ""
	}
	return false, tmpDevs, common.GenReason(reason, len(devices))
}
