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

// Where a pod's compute units are recorded.
//
// The obvious places do not work, and both were measured rather than reasoned
// about:
//
//   - Not in ContainerDevice.CustomInfo. EncodeContainerDevices writes only
//     UUID,Type,Usedmem,Usedcores, so the moment HAMi rebuilds its state by
//     decoding the pod annotation the offsets are gone. Observed as two pods
//     receiving the identical mask while usedcores was correctly 8.
//   - Not in a "UUID[...]" suffix. A '[' in a device UUID is HAMi's marker for
//     a MIG instance; the suffix drives MIG code paths and panics the
//     scheduler with an index out of range.
//   - Not read back off the placed pods. PodInfo.Pod is the pod as it arrived
//     at the extender's filter request: phase Pending, carrying only what
//     admission wrote. Neither this mask nor HAMi's own
//     hami.io/amd-devices-allocated is on it, because both are patched after
//     the filter returns.
//
// So the offsets get storage of their own, on the node, keyed by pod UID.
// Liveness is not tracked here: an entry whose pod is no longer among the
// device's PodInfos is simply dropped, which makes the record self-pruning and
// means a crashed scheduler cannot leak compute units permanently.

import (
	"encoding/json"
	"sync"
	"time"
)

// CUAllocAnnotation holds the per-pod compute unit assignments for every AMD
// GPU on a node, as {"<pod uid>": {"dev": "<device id>", "mask": "0x3f"}}.
const CUAllocAnnotation = "hami.io/node-amd-cu-allocations"

// inflightTTL bounds how long an assignment is remembered in process before
// the node annotation is trusted on its own. It only has to cover the gap
// between writing the annotation and that write becoming visible through the
// informer cache; a filter 8ms after a bind was measured, so this is generous.
const inflightTTL = 2 * time.Minute

type cuAllocation struct {
	Device string `json:"dev"`
	Mask   string `json:"mask"`
}

// parseAllocations reads the annotation. A value that cannot be parsed yields
// no allocations, and the caller treats "no allocations" conservatively by
// falling back to what it can see in flight — it never treats it as "the
// device is empty".
func parseAllocations(s string) map[string]cuAllocation {
	if s == "" {
		return map[string]cuAllocation{}
	}
	out := map[string]cuAllocation{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return map[string]cuAllocation{}
	}
	return out
}

func formatAllocations(m map[string]cuAllocation) (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// prune drops entries for pods that are no longer on the device. live is the
// set of pod UIDs currently placed there, which HAMi maintains in PodInfos.
func prune(m map[string]cuAllocation, live map[string]bool) map[string]cuAllocation {
	out := make(map[string]cuAllocation, len(m))
	for uid, a := range m {
		if live[uid] {
			out[uid] = a
		}
	}
	return out
}

// occupiedFrom unions the masks recorded against one device.
//
// A mask that cannot be parsed makes the whole device look occupied. Refusing
// to place a pod is visible and recoverable; handing out compute units that
// another sandbox is already running on is neither, because a queue mask is
// fixed when the queue is created and cannot be taken back.
func occupiedFrom(m map[string]cuAllocation, deviceID string, total int) (cuSet, bool) {
	var occupied cuSet
	for _, a := range m {
		if a.Device != deviceID || a.Mask == "" {
			continue
		}
		mask, err := parseCUMask(a.Mask)
		if err != nil {
			return nil, false
		}
		occupied.or(mask)
	}
	return occupied, true
}

// inflightStore remembers assignments this process has just made, covering the
// window where the node annotation has been written but the informer cache has
// not caught up. Without it, two pods submitted together are both handed the
// lowest free run — measured, 8ms apart.
type inflightStore struct {
	mu sync.Mutex
	m  map[string]inflightEntry
}

type inflightEntry struct {
	alloc cuAllocation
	at    time.Time
}

func newInflightStore() *inflightStore {
	return &inflightStore{m: map[string]inflightEntry{}}
}

func (s *inflightStore) add(uid, device, mask string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[uid] = inflightEntry{alloc: cuAllocation{Device: device, Mask: mask}, at: time.Now()}
}

// snapshot returns the live entries for a device, expiring stale ones. An
// entry expires rather than being deleted on failure because a bind that never
// completes would otherwise hold compute units for the process's lifetime.
func (s *inflightStore) snapshot(deviceID string, now time.Time) map[string]cuAllocation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]cuAllocation{}
	for uid, e := range s.m {
		if now.Sub(e.at) > inflightTTL {
			delete(s.m, uid)
			continue
		}
		if e.alloc.Device == deviceID {
			out[uid] = e.alloc
		}
	}
	return out
}

// merge folds b into a, preferring a. Used to overlay in-flight assignments on
// what the node annotation says.
func merge(a, b map[string]cuAllocation) map[string]cuAllocation {
	out := make(map[string]cuAllocation, len(a)+len(b))
	for k, v := range b {
		out[k] = v
	}
	for k, v := range a {
		out[k] = v
	}
	return out
}

// registeredDevice mirrors what the AMD device plugin publishes as
// hami.io/node-amd-register, in the shape HAMi's NVIDIA plugin uses for
// hami.io/node-nvidia-register.
type registeredDevice struct {
	ID      string `json:"id"`
	Count   int    `json:"count"`
	DevMem  int    `json:"devmem"`
	DevCore int    `json:"devcore"`
	CUGroup int    `json:"cugroup"`
	Type    string `json:"type"`
	Health  bool   `json:"health"`
	NodeIdx int    `json:"nodeindex"`
}

// RegisterAnnotation is where the AMD device plugin publishes the node's GPUs.
const RegisterAnnotation = "hami.io/node-amd-register"

// cuGroupKey carries the queue-mask granularity from the plugin's registration
// through to Fit. The plugin derives it from gfx_target_version, which is the
// only place the truth lives: RDNA pairs compute units and KFD refuses a mask
// that splits a pair, CDNA does not pair, and guessing wrong makes containers
// hang rather than fail.
const cuGroupKey = "gvisorCUGroup"

func parseRegistration(s string) ([]registeredDevice, error) {
	if s == "" {
		return nil, nil
	}
	var out []registeredDevice
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}
