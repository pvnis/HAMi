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
	"fmt"
	"strings"
)

// A CU mask selects which of a GPU's compute units a queue may run on. AMD's
// KFD applies it at queue creation and the hardware honours it thereafter, so
// unlike a time-sliced share it needs no ongoing decision — but it is only a
// partition if the masks handed to different sandboxes do not overlap, and
// nothing enforces that except whatever assigns them. That is what this file
// is for.
//
// The mask is written as a hexadecimal integer with compute unit 0 in the
// least significant bit, which is the form amdproxy's --amdproxy-cu-mask takes.

// cuSet is a bitset over compute units, least significant bit first. A GPU can
// have more than 64 (MI300X has 304), so this cannot be a single word.
type cuSet []uint64

func newCUSet(n int) cuSet {
	if n <= 0 {
		return cuSet{}
	}
	return make(cuSet, (n+63)/64)
}

func (s cuSet) has(i int) bool {
	w := i / 64
	if w >= len(s) {
		return false
	}
	return s[w]&(1<<uint(i%64)) != 0
}

func (s *cuSet) set(i int) {
	w := i / 64
	for len(*s) <= w {
		*s = append(*s, 0)
	}
	(*s)[w] |= 1 << uint(i%64)
}

func (s *cuSet) or(o cuSet) {
	for w, v := range o {
		for len(*s) <= w {
			*s = append(*s, 0)
		}
		(*s)[w] |= v
	}
}

func (s cuSet) empty() bool {
	for _, w := range s {
		if w != 0 {
			return false
		}
	}
	return true
}

func (s cuSet) count() int {
	n := 0
	for _, w := range s {
		for ; w != 0; w &= w - 1 {
			n++
		}
	}
	return n
}

// String renders the set as amdproxy expects it: "0x" and hexadecimal, most
// significant word first, with no leading zeroes.
func (s cuSet) String() string {
	hi := len(s) - 1
	for hi > 0 && s[hi] == 0 {
		hi--
	}
	if hi < 0 {
		return "0x0"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "0x%x", s[hi])
	for w := hi - 1; w >= 0; w-- {
		fmt.Fprintf(&b, "%016x", s[w])
	}
	return b.String()
}

// parseCUMask reads a mask in the form amdproxy accepts. Unlike amdproxy's own
// parser this is only ever fed masks it wrote itself or that an operator put on
// a pod, and a mask it cannot read is treated by the caller as "unknown
// occupancy", which is the safe direction: it is counted as fully occupied
// rather than free.
func parseCUMask(s string) (cuSet, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty mask")
	}
	digits := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if digits == "" {
		return nil, fmt.Errorf("%q has no hexadecimal digits", s)
	}
	if len(digits) > 256 {
		return nil, fmt.Errorf("%q describes more than 1024 compute units", s)
	}
	var out cuSet
	// Read 16 hex digits (one 64-bit word) at a time from the least
	// significant end.
	for end := len(digits); end > 0; end -= 16 {
		start := end - 16
		if start < 0 {
			start = 0
		}
		var w uint64
		for _, r := range digits[start:end] {
			d, ok := hexVal(r)
			if !ok {
				return nil, fmt.Errorf("%q is not a hexadecimal integer", s)
			}
			w = w<<4 | uint64(d)
		}
		out = append(out, w)
	}
	return out, nil
}

func hexVal(r rune) (int, bool) {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0'), true
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10, true
	case r >= 'A' && r <= 'F':
		return int(r-'A') + 10, true
	}
	return 0, false
}

// cusForRequest converts a share of a GPU's memory into a number of compute
// units, rounded down to whole groups.
//
// Deriving compute from the memory request keeps an AMD slice one-dimensional:
// a pod asking for a quarter of the VRAM gets about a quarter of the compute,
// with no second resource to declare. A request always yields at least one
// group, since a queue with no compute units cannot run at all.
func cusForRequest(wantMem, totalMem int64, totalCUs, group int) int {
	if group < 1 {
		group = 1
	}
	if totalMem <= 0 || wantMem <= 0 || totalCUs < group {
		return group
	}
	cus := int(int64(totalCUs) * wantMem / totalMem)
	cus -= cus % group
	if cus < group {
		cus = group
	}
	if cus > totalCUs-(totalCUs%group) {
		cus = totalCUs - (totalCUs % group)
	}
	return cus
}

// allocateCUs picks the lowest run of want compute units that does not overlap
// occupied, aligned to group.
//
// RDNA pairs compute units into workgroup processors and KFD returns EINVAL for
// a queue mask that enables half a pair, so a mask that is not group-aligned is
// not merely suboptimal — it makes every queue creation fail, which amdproxy
// reports by destroying the queue and which ROCr turns into a null dereference.
// The operator sees a container that hangs. Alignment is therefore a
// correctness requirement, not a tuning choice.
//
// Runs are contiguous. A fragmented allocation would still be a valid mask, but
// contiguity keeps a tenant's units on as few shader engines as possible and
// makes the assignment easy to read in a log.
func allocateCUs(occupied cuSet, total, want, group int) (cuSet, bool) {
	if group < 1 {
		group = 1
	}
	if want <= 0 || total <= 0 || want > total {
		return nil, false
	}
	// Only whole groups may be handed out, and the run must start on a group
	// boundary.
	if want%group != 0 {
		want += group - want%group
	}
	for start := 0; start+want <= total; start += group {
		free := true
		for i := start; i < start+want; i++ {
			if occupied.has(i) {
				free = false
				break
			}
		}
		if !free {
			continue
		}
		out := newCUSet(total)
		for i := start; i < start+want; i++ {
			out.set(i)
		}
		return out, true
	}
	return nil, false
}
