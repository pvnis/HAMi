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
	"time"

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
	inflight           *inflightStore
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
		inflight:           newInflightStore(),
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
	// The AMD device plugin publishes the card's shape - framebuffer size,
	// compute unit count and queue-mask granularity - read out of the KFD
	// topology, exactly as HAMi's NVIDIA plugin publishes
	// hami.io/node-nvidia-register. Prefer that over anything hand-set: the
	// labels below are a fallback for a plugin too old to publish it.
	if regd, err := parseRegistration(n.Annotations[RegisterAnnotation]); err == nil && len(regd) > 0 {
		nodedevices := []*device.DeviceInfo{}
		for _, r := range regd {
			nodedevices = append(nodedevices, &device.DeviceInfo{
				Index:        uint(r.NodeIdx),
				ID:           r.ID,
				Count:        int32(r.Count),
				Devmem:       int32(r.DevMem),
				Devcore:      int32(r.DevCore),
				Type:         AMDDevice,
				Numa:         0,
				Health:       r.Health,
				CustomInfo:   map[string]any{cuGroupKey: r.CUGroup},
				DeviceVendor: AMDCommonWord,
			})
		}
		klog.V(4).InfoS("registered AMD node devices from the plugin", "node", n.Name, "devices", len(nodedevices))
		return nodedevices, nil
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
// PatchAnnotations records the allocation on the pod and on the node.
//
// The pod gets the CU mask as a gVisor flag, which is what actually enforces
// the compute partition: amdproxy applies it to every queue the sandbox
// creates and the GPU's command processor honours it, so the sandbox cannot
// widen it from inside.
//
// The node gets the same mask keyed by pod UID, because the pod annotation
// cannot be read back — see cualloc.go. A failure to write the node record is
// logged and not fatal: the pod still gets a correct, non-overlapping mask,
// and the in-process overlay covers the gap until the next successful write.
func (dev *AMDDevices) PatchAnnotations(pod *corev1.Pod, annoinput *map[string]string, pd device.PodDevices) map[string]string {
	devlist, ok := pd[AMDDevice]
	if !ok || len(devlist) == 0 {
		return *annoinput
	}
	(*annoinput)[device.SupportDevices[AMDDevice]] = device.EncodePodSingleDevice(devlist)

	for _, ctrdevs := range devlist {
		for _, cd := range ctrdevs {
			mask, ok := cd.CustomInfo[cuMaskKey].(string)
			if !ok || mask == "" {
				continue
			}
			(*annoinput)[CUMaskAnnotation] = mask
			klog.InfoS("assigned AMD compute units", "pod", klog.KObj(pod), "device", cd.UUID, "cuMask", mask)

			nodeName := nodeNameOf(cd.UUID)
			if nodeName == "" {
				klog.ErrorS(nil, "cannot derive a node name from the device ID; the CU allocation will not be recorded",
					"device", cd.UUID)
				continue
			}
			if err := recordCUAllocation(nodeName, string(pod.UID), cd.UUID, mask); err != nil {
				klog.ErrorS(err, "recording the CU allocation on the node",
					"node", nodeName, "pod", klog.KObj(pod), "cuMask", mask)
			}
		}
	}
	return *annoinput
}

// nodeNameOf recovers the node from a device ID of the form
// "<node>-AMDGPU-<index>", which is how both this package and the AMD device
// plugin construct it.
func nodeNameOf(deviceID string) string {
	i := strings.LastIndex(deviceID, "-"+AMDDevice+"-")
	if i <= 0 {
		return ""
	}
	return deviceID[:i]
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
// occupiedCUs returns the compute units already handed out on a device, from
// the node's allocation record overlaid with anything this process has just
// assigned. live is the set of pod UIDs currently placed on the device, used
// to drop records for pods that have gone.
//
// The second return is false when occupancy cannot be determined, in which
// case the caller must refuse to place rather than assume the device is free.
func (amddevice *AMDDevices) occupiedCUs(node *corev1.Node, dev *device.DeviceUsage, total int) (cuSet, bool) {
	live := map[string]bool{}
	for _, pi := range dev.PodInfos {
		if pi != nil && pi.Pod != nil {
			live[string(pi.Pod.UID)] = true
		}
	}

	recorded := map[string]cuAllocation{}
	if node != nil {
		recorded = prune(parseAllocations(node.Annotations[CUAllocAnnotation]), live)
	}
	all := merge(amddevice.inflight.snapshot(dev.ID, time.Now()), recorded)

	occupied, ok := occupiedFrom(all, dev.ID, total)
	if !ok {
		klog.ErrorS(nil, "unreadable CU allocation record; refusing to place rather than risk overlapping masks",
			"device", dev.ID)
		return nil, false
	}
	return occupied, true
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
		var nodeObj *corev1.Node
		if nodeinfo != nil {
			nodeObj = nodeinfo.Node
		}
		occ, readable := amddevice.occupiedCUs(nodeObj, dev, total)
		if !readable {
			reason[common.CardInsufficientCore]++
			continue
		}
		group := amddevice.cuGroupSize
		if g, ok := dev.CustomInfo[cuGroupKey].(int); ok && g > 0 {
			group = g
		}
		klog.V(4).InfoS("AMD CU occupancy", "pod", klog.KObj(pod), "device", dev.ID,
			"occupied", occ.String(), "occupiedCount", occ.count(), "wantCUs", want, "group", group)
		mask, ok := allocateCUs(occ, total, want, group)
		if !ok {
			reason[common.CardInsufficientCore]++
			klog.V(5).InfoS(common.CardInsufficientCore, "pod", klog.KObj(pod), "device", dev.ID,
				"wantCUs", want, "totalCUs", total)
			continue
		}

		amddevice.inflight.add(string(pod.UID), dev.ID, mask.String())
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
