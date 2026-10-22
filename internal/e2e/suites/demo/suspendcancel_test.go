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
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A client that gives up on a synchronous suspend must still get a resumable
// actor: the checkpoint atelet started finishes and the actor ends SUSPENDED,
// whether the client waits or retries the suspend.
func TestCancelledSyncSuspendLeavesResumableActor(t *testing.T) {
	t.Parallel()
	nsObj := e2e.CreateNamespace(t)
	ctx := context.Background()
	clients := e2e.GetClients()

	at, err := createActorTemplate(ctx, t, clients, nsObj, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	if err != nil {
		t.Fatalf("failed to initialize ActorTemplate: %v", err)
	}

	tests := []struct {
		name  string
		retry bool
	}{
		{name: "client waits", retry: false},
		{name: "client retries the suspend", retry: true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actorName := "cancel-" + string(rune('a'+i)) + "-" + nsObj.Name
			ref := &ateapipb.ObjectRef{Atespace: demoAtespace, Name: actorName}
			createAndResumeActor(ctx, t, clients, at, actorName)
			resp, err := callActor(t, resources.ActorRef{Atespace: demoAtespace, Name: actorName})
			if err != nil {
				t.Fatalf("failed to call actor: %v", err)
			}
			validateCounterResponse(t, resp, "before suspend", 1, 1)

			suspendCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := clients.SubstrateAPI.SuspendActor(suspendCtx, &ateapipb.SuspendActorRequest{
					Actor:   ref,
					Options: &ateapipb.SuspendOptions{Mode: ateapipb.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS},
				})
				done <- err
			}()
			// Cancel as soon as the actor is marked SUSPENDING, which is just
			// before ateapi calls atelet. A memory checkpoint plus upload
			// takes seconds, so the suspend finishing first means the cancel
			// was never exercised.
			for deadline := time.Now().Add(30 * time.Second); ; {
				select {
				case err := <-done:
					t.Fatalf("SuspendActor finished before SUSPENDING was observed (%v); cancel not exercised", err)
				case <-time.After(50 * time.Millisecond):
				}
				if getActor(ctx, t, clients, actorName).GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_SUSPENDING {
					cancel()
					t.Logf("SuspendActor after cancel: %v", <-done)
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("Actor %q never observed SUSPENDING", actorName)
				}
			}

			if tc.retry {
				retryCtx, retryCancel := context.WithTimeout(ctx, 2*time.Minute)
				defer retryCancel()
				_, err := clients.SubstrateAPI.SuspendActor(retryCtx, &ateapipb.SuspendActorRequest{
					Actor:   ref,
					Options: &ateapipb.SuspendOptions{Mode: ateapipb.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS},
				})
				// Aborted is the first suspend still holding the actor's lease.
				if err != nil && status.Code(err) != codes.Aborted {
					t.Errorf("SuspendActor retry = %v, want OK or Aborted", err)
				}
			}

			waitForActorStateWithTimeout(ctx, t, clients, actorName, ateapipb.ActorState_ACTOR_STATE_SUSPENDED, 2*time.Minute)
			resumeActorRetrying(ctx, t, clients, actorName, 2*time.Minute)
			resp, err = callActor(t, resources.ActorRef{Atespace: demoAtespace, Name: actorName})
			if err != nil {
				t.Fatalf("failed to call actor after resume: %v", err)
			}
			validateCounterResponse(t, resp, "after resume", 2, 2)
		})
	}
}
