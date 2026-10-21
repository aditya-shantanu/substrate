// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package demo

import (
	"context"
	"testing"
	"time"

	"go.ate.dev/substrate/internal/e2e"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// DeleteActor converges once the actor's worker pod is gone, whether the
// crash is already recorded or the pod is still disappearing. Retries may be
// needed in the second case, but the actor must not stay DELETING.
func TestDeleteActorWhoseWorkerIsGone(t *testing.T) {
	t.Parallel()
	nsObj := e2e.CreateNamespace(t)
	ctx := context.Background()
	clients := e2e.GetClients()

	at, err := createActorTemplate(ctx, t, clients, nsObj, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	if err != nil {
		t.Fatalf("failed to initialize ActorTemplate: %v", err)
	}

	tests := []struct {
		name string
		// waitForCrash deletes the pod gracefully and lets the control plane
		// record the crash first; otherwise the pod is force-deleted and the
		// actor deleted at once.
		waitForCrash bool
	}{
		{name: "after the crash is recorded", waitForCrash: true},
		{name: "while the pod is still going away", waitForCrash: false},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actorName := "delete-gone-" + string(rune('a'+i)) + "-" + nsObj.Name
			ref := &ateapipb.ObjectRef{Atespace: demoAtespace, Name: actorName}
			running := createAndResumeActor(ctx, t, clients, at, actorName)
			podNS := running.GetStatus().GetWorkerAssignment().GetWorkerNamespace()
			podName := running.GetStatus().GetWorkerAssignment().GetWorkerPod()

			opts := metav1.DeleteOptions{}
			if !tc.waitForCrash {
				opts.GracePeriodSeconds = ptr.To[int64](0)
			}
			t.Logf("Deleting worker pod %s/%s", podNS, podName)
			if err := clients.K8s.CoreV1().Pods(podNS).Delete(ctx, podName, opts); err != nil {
				t.Fatalf("failed to delete worker pod %s/%s: %v", podNS, podName, err)
			}
			if tc.waitForCrash {
				waitForActorState(ctx, t, clients, actorName, ateapipb.ActorState_ACTOR_STATE_CRASHED)
				if _, err := clients.SubstrateAPI.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref}); err != nil {
					t.Fatalf("DeleteActor of crashed actor %q: %v", actorName, err)
				}
			} else {
				deleteActorRetrying(ctx, t, clients, actorName, 90*time.Second)
			}

			if _, err := clients.SubstrateAPI.GetActor(ctx, &ateapipb.GetActorRequest{Actor: ref}); status.Code(err) != codes.NotFound {
				t.Fatalf("GetActor %q after delete = %v, want NotFound", actorName, err)
			}
		})
	}
}

// deleteActorRetrying calls DeleteActor until it succeeds or the actor is
// gone, reporting the last error if that takes longer than timeout.
func deleteActorRetrying(ctx context.Context, t *testing.T, clients *e2e.Clients, actorName string, timeout time.Duration) {
	t.Helper()
	ref := &ateapipb.ObjectRef{Atespace: demoAtespace, Name: actorName}
	deadline := time.Now().Add(timeout)
	for {
		_, err := clients.SubstrateAPI.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref})
		if err == nil || status.Code(err) == codes.NotFound {
			return
		}
		t.Logf("DeleteActor %q: %v; retrying", actorName, err)
		if time.Now().After(deadline) {
			actor, getErr := clients.SubstrateAPI.GetActor(ctx, &ateapipb.GetActorRequest{Actor: ref})
			t.Fatalf("DeleteActor %q still failing after %v: %v (actor %v, GetActor err %v)", actorName, timeout, err, actor.GetStatus().GetState(), getErr)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("DeleteActor %q: %v", actorName, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
