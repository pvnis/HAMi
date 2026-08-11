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

import "testing"

func TestParseAndFormatRoundTrip(t *testing.T) {
	for _, s := range []string{"0x1", "0x3f", "0xffffffff", "0x3fffffffffffff", "0x1ffffffffffffffff"} {
		set, err := parseCUMask(s)
		if err != nil {
			t.Fatalf("parseCUMask(%q): %v", s, err)
		}
		if got := set.String(); got != s {
			t.Errorf("round trip of %q gave %q", s, got)
		}
	}
}

func TestParseCUMaskRejectsGarbage(t *testing.T) {
	// "all" begins with a valid hex digit, which is the case a parser that
	// stops at the first bad character silently accepts as 0xa.
	for _, s := range []string{"", "0x", "all", "0xzz", "12g4", "-1"} {
		if _, err := parseCUMask(s); err == nil {
			t.Errorf("parseCUMask(%q) = nil error, want an error", s)
		}
	}
}

func TestParseCUMaskWideValues(t *testing.T) {
	// A 304-CU MI300X needs five 64-bit words; the mask must not be truncated.
	set, err := parseCUMask("0x" + "f" + "0000000000000000" + "0000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if !set.has(128) || !set.has(131) || set.has(127) || set.has(132) {
		t.Errorf("wide mask parsed wrong: %v", set)
	}
}

func TestCUsForRequest(t *testing.T) {
	// sens1: 54 CUs, 23 slices of 512 MiB, RDNA so groups of 2.
	for _, tc := range []struct {
		name                        string
		wantMem, totalMem           int64
		totalCUs, group, wantResult int
	}{
		{name: "quarter of memory gets about a quarter of compute", wantMem: 6, totalMem: 23, totalCUs: 54, group: 2, wantResult: 14},
		{name: "half", wantMem: 12, totalMem: 23, totalCUs: 54, group: 2, wantResult: 28},
		{name: "everything", wantMem: 23, totalMem: 23, totalCUs: 54, group: 2, wantResult: 54},
		{name: "a small request still gets a whole group", wantMem: 1, totalMem: 23, totalCUs: 54, group: 2, wantResult: 2},
		{name: "result is always group aligned", wantMem: 4, totalMem: 23, totalCUs: 54, group: 2, wantResult: 8},
		{name: "no pairing on CDNA", wantMem: 4, totalMem: 23, totalCUs: 54, group: 1, wantResult: 9},
		{name: "an odd CU count cannot overrun", wantMem: 23, totalMem: 23, totalCUs: 13, group: 2, wantResult: 12},
		{name: "degenerate input still yields a runnable mask", wantMem: 0, totalMem: 0, totalCUs: 54, group: 2, wantResult: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cusForRequest(tc.wantMem, tc.totalMem, tc.totalCUs, tc.group)
			if got != tc.wantResult {
				t.Errorf("cusForRequest(%d/%d of %d CUs, group %d) = %d, want %d",
					tc.wantMem, tc.totalMem, tc.totalCUs, tc.group, got, tc.wantResult)
			}
			if got%tc.group != 0 {
				t.Errorf("result %d is not a multiple of the group size %d", got, tc.group)
			}
		})
	}
}

func TestAllocateCUsIsDisjointAndAligned(t *testing.T) {
	var occupied cuSet

	// First tenant takes the lowest 6.
	a, ok := allocateCUs(occupied, 54, 6, 2)
	if !ok {
		t.Fatal("first allocation failed")
	}
	if a.String() != "0x3f" {
		t.Errorf("first allocation = %s, want 0x3f", a.String())
	}
	occupied.or(a)

	// Second tenant must not overlap the first.
	b, ok := allocateCUs(occupied, 54, 6, 2)
	if !ok {
		t.Fatal("second allocation failed")
	}
	if b.String() != "0xfc0" {
		t.Errorf("second allocation = %s, want 0xfc0", b.String())
	}
	for i := 0; i < 54; i++ {
		if a.has(i) && b.has(i) {
			t.Fatalf("CU %d handed to both tenants", i)
		}
	}
	occupied.or(b)

	// A request that no longer fits must be refused rather than overlapping.
	if _, ok := allocateCUs(occupied, 54, 48, 2); ok {
		t.Error("allocated 48 CUs with only 42 free")
	}
	// ...but one that does fit still succeeds.
	if _, ok := allocateCUs(occupied, 54, 42, 2); !ok {
		t.Error("failed to allocate the 42 CUs that remain")
	}
}

func TestAllocateCUsAlignment(t *testing.T) {
	// A request of 3 on a paired GPU must be rounded up to 4, never granted as
	// 3: KFD rejects a mask that enables half a workgroup processor, and the
	// container hangs rather than failing cleanly.
	got, ok := allocateCUs(nil, 54, 3, 2)
	if !ok {
		t.Fatal("allocation failed")
	}
	if got.count() != 4 {
		t.Errorf("granted %d CUs for a request of 3, want 4", got.count())
	}
	if got.String() != "0xf" {
		t.Errorf("got %s, want 0xf", got.String())
	}
}

func TestAllocateCUsFillsGapsLeftByDepartedTenants(t *testing.T) {
	// Two tenants at 0-5 and 6-11; the first leaves. A new request for 6 must
	// reuse the freed low range rather than fragmenting further up.
	occupied, err := parseCUMask("0xfc0") // only the second tenant remains
	if err != nil {
		t.Fatal(err)
	}
	got, ok := allocateCUs(occupied, 54, 6, 2)
	if !ok {
		t.Fatal("allocation failed")
	}
	if got.String() != "0x3f" {
		t.Errorf("got %s, want the freed 0x3f", got.String())
	}
}

func TestAllocateCUsRefusesImpossibleRequests(t *testing.T) {
	if _, ok := allocateCUs(nil, 54, 0, 2); ok {
		t.Error("allocated zero CUs")
	}
	if _, ok := allocateCUs(nil, 54, 60, 2); ok {
		t.Error("allocated more CUs than the device has")
	}
}
