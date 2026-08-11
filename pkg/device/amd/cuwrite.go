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
	"context"
	"fmt"
	"time"

	"github.com/Project-HAMi/HAMi/pkg/util/client"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// recordCUAllocation adds one pod's compute unit assignment to the node.
//
// Two binds can land on the same node at once, and a blind overwrite would
// drop one of their records — the pod would keep running with a mask nobody
// remembers, and the next pod would be handed the same compute units. So this
// is a read-modify-write against the object's resourceVersion, retried on
// conflict, rather than a merge patch of the whole annotation.
func recordCUAllocation(nodeName, podUID, deviceID, mask string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if client.GetClient() == nil {
		return fmt.Errorf("no Kubernetes client configured")
	}

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		node, err := client.GetClient().CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting node %s: %w", nodeName, err)
		}
		current := parseAllocations(node.Annotations[CUAllocAnnotation])
		if existing, ok := current[podUID]; ok && existing.Mask == mask && existing.Device == deviceID {
			return nil // already recorded, nothing to do
		}
		current[podUID] = cuAllocation{Device: deviceID, Mask: mask}

		encoded, err := formatAllocations(current)
		if err != nil {
			return err
		}
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}
		node.Annotations[CUAllocAnnotation] = encoded

		// Update, not Patch: Update carries the resourceVersion that was read,
		// so a concurrent writer causes a conflict here instead of silently
		// winning.
		if _, err = client.GetClient().CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err == nil {
			return nil
		}
		lastErr = err
		klog.V(4).InfoS("CU allocation record conflicted, retrying", "node", nodeName, "attempt", attempt+1)
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}
	return fmt.Errorf("recording CU allocation on node %s after retries: %w", nodeName, lastErr)
}
