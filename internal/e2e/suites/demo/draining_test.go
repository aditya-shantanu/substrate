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
	"strings"
	"sync"
	"testing"
	"time"

	"go.ate.dev/substrate/internal/e2e"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// A paused actor is pinned to the node that holds its snapshot. When the
// worker it ran on is draining, the resume has to go to another worker, on
// that node or after a detach, and must never crash the actor.
func TestResumeAfterWorkerDrained(t *testing.T) {
	t.Parallel()
	nsObj := e2e.CreateNamespace(t)
	ctx := context.Background()
	clients := e2e.GetClients()

	at, err := createActorTemplate(ctx, t, clients, nsObj, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	if err != nil {
		t.Fatalf("failed to initialize ActorTemplate: %v", err)
	}

	actorName := "drained-" + nsObj.Name
	running := createAndResumeActor(ctx, t, clients, at, actorName)
	resp, err := callActor(t, resources.ActorRef{Atespace: demoAtespace, Name: actorName})
	if err != nil {
		t.Fatalf("failed to call actor: %v", err)
	}
	validateCounterResponse(t, resp, "before pause", 1, 1)
	worker := running.GetStatus().GetWorkerAssignment().GetWorker().GetName()

	pauseActorAndWait(ctx, t, clients, actorName)

	t.Logf("Draining worker %s (%s)", worker, running.GetStatus().GetWorkerAssignment().GetWorkerPod())
	if _, err := clients.SubstrateAPI.DrainWorker(ctx, &ateapipb.DrainWorkerRequest{Worker: &ateapipb.ObjectRef{Name: worker}}); err != nil {
		t.Fatalf("DrainWorker %s: %v", worker, err)
	}

	resumeActorRetrying(ctx, t, clients, actorName, 2*time.Minute)
	resumed := getActor(ctx, t, clients, actorName)
	if got := resumed.GetStatus().GetWorkerAssignment().GetWorker().GetName(); got == worker {
		t.Errorf("actor resumed on draining worker %s", worker)
	}
	resp, err = callActor(t, resources.ActorRef{Atespace: demoAtespace, Name: actorName})
	if err != nil {
		t.Fatalf("failed to call actor after resume: %v", err)
	}
	validateCounterResponse(t, resp, "after resume", 2, 2)
}

// Scaling the pool down while actors are restoring must not crash them. A
// resume caught by the drain either completes, and the actor later crashes
// like any actor whose pod goes away, or fails with a retriable code and
// leaves the actor SUSPENDED with its snapshot.
//
// The window is the restore, fetching the durable snapshot included, so one
// round does not always hit it.
func TestScaleDownDuringResume(t *testing.T) {
	t.Parallel()
	nsObj := e2e.CreateNamespace(t)
	ctx := context.Background()
	clients := e2e.GetClients()

	at, err := createActorTemplate(ctx, t, clients, nsObj, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	if err != nil {
		t.Fatalf("failed to initialize ActorTemplate: %v", err)
	}

	// One actor per worker of the pool createActorTemplate makes.
	names := []string{"scaledown-a-" + nsObj.Name, "scaledown-b-" + nsObj.Name}
	for _, name := range names {
		createAndResumeActor(ctx, t, clients, at, name)
	}
	for _, name := range names {
		if _, err := clients.SubstrateAPI.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
			Actor:   &ateapipb.ObjectRef{Atespace: demoAtespace, Name: name},
			Options: &ateapipb.SuspendOptions{Mode: ateapipb.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS},
		}); err != nil {
			t.Fatalf("failed to suspend Actor %q: %v", name, err)
		}
		waitForActorState(ctx, t, clients, name, ateapipb.ActorState_ACTOR_STATE_SUSPENDED)
	}

	// The resumes race the pods' SIGTERM: a worker that is still ACTIVE when
	// the scheduler reads it may be terminating by the time it restores.
	t.Logf("Scaling WorkerPool %s/counter to 0 and resuming %d actors", nsObj.Name, len(names))
	if _, err := clients.SubstrateK8s.ApiV1alpha1().WorkerPools(nsObj.Name).Patch(ctx, "counter", types.MergePatchType,
		[]byte(`{"spec":{"replicas":0}}`), metav1.PatchOptions{}); err != nil {
		t.Fatalf("scaling WorkerPool to 0: %v", err)
	}
	resumeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = clients.SubstrateAPI.ResumeActor(resumeCtx, &ateapipb.ResumeActorRequest{
				Actor: &ateapipb.ObjectRef{Atespace: demoAtespace, Name: name},
			})
		}()
	}
	wg.Wait()

	for i, name := range names {
		switch code := status.Code(errs[i]); code {
		case codes.OK:
			t.Logf("ResumeActor %s succeeded", name)
		case codes.ResourceExhausted, codes.Unavailable, codes.Aborted:
			t.Logf("ResumeActor %s refused: %v", name, errs[i])
		default:
			t.Errorf("ResumeActor %s = %v, want OK or a retriable code", name, errs[i])
		}
		actor := waitForActorSettled(ctx, t, clients, name, 2*time.Minute)
		switch actor.GetStatus().GetState() {
		case ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
			if !hasLocalSnapshot(actor.GetStatus()) && durableSnapshotURI(actor.GetStatus()) == "" {
				t.Errorf("actor %s is SUSPENDED without a snapshot: %v", name, actor.GetStatus())
			}
		case ateapipb.ActorState_ACTOR_STATE_CRASHED:
			if msg := actor.GetStatus().GetCrash().GetMessage(); strings.HasPrefix(msg, "resume failed: ") {
				t.Errorf("actor %s crashed by the drained resume: %s", name, msg)
			}
		}
	}
}

func getActor(ctx context.Context, t *testing.T, clients *e2e.Clients, actorName string) *ateapipb.Actor {
	t.Helper()
	actor, err := clients.SubstrateAPI.GetActor(ctx, &ateapipb.GetActorRequest{
		Actor: &ateapipb.ObjectRef{Atespace: demoAtespace, Name: actorName},
	})
	if err != nil {
		t.Fatalf("GetActor %q: %v", actorName, err)
	}
	return actor
}

// createAndResumeActor creates an actor from at, resumes it and returns the
// RUNNING record. The actor is deleted when the test ends.
func createAndResumeActor(ctx context.Context, t *testing.T, clients *e2e.Clients, at *ateapipb.ActorTemplate, actorName string) *ateapipb.Actor {
	t.Helper()
	ref := &ateapipb.ObjectRef{Atespace: demoAtespace, Name: actorName}
	if _, err := clients.SubstrateAPI.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: demoAtespace, Name: actorName},
		ActorTemplate: e2e.TemplateRef(at),
	}}); err != nil {
		t.Fatalf("failed to create Actor %q: %v", actorName, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := clients.SubstrateAPI.DeleteActor(cleanupCtx, &ateapipb.DeleteActorRequest{Actor: ref}); err != nil && status.Code(err) != codes.NotFound {
			t.Logf("failed to delete Actor %q: %v", actorName, err)
		}
	})
	waitForActorState(ctx, t, clients, actorName, ateapipb.ActorState_ACTOR_STATE_SUSPENDED)
	if _, err := e2e.ResumeActorAwaitCapacity(t, ctx, clients, &ateapipb.ResumeActorRequest{Actor: ref}); err != nil {
		t.Fatalf("failed to resume Actor %q: %v", actorName, err)
	}
	waitForActorState(ctx, t, clients, actorName, ateapipb.ActorState_ACTOR_STATE_RUNNING)
	return getActor(ctx, t, clients, actorName)
}

func pauseActorAndWait(ctx context.Context, t *testing.T, clients *e2e.Clients, actorName string) {
	t.Helper()
	if _, err := clients.SubstrateAPI.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
		Actor:   &ateapipb.ObjectRef{Atespace: demoAtespace, Name: actorName},
		Options: &ateapipb.SuspendOptions{Mode: ateapipb.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS},
	}); err != nil {
		t.Fatalf("failed to pause Actor %q: %v", actorName, err)
	}
	waitForActorPaused(ctx, t, clients, actorName)
}

// resumeActorRetrying resumes the actor, retrying the codes a caller is meant
// to retry, until it is RUNNING. A crash fails the test at once.
func resumeActorRetrying(ctx context.Context, t *testing.T, clients *e2e.Clients, actorName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		_, err := clients.SubstrateAPI.ResumeActor(ctx, &ateapipb.ResumeActorRequest{
			Actor: &ateapipb.ObjectRef{Atespace: demoAtespace, Name: actorName},
		})
		if err == nil {
			waitForActorState(ctx, t, clients, actorName, ateapipb.ActorState_ACTOR_STATE_RUNNING)
			return
		}
		switch status.Code(err) {
		case codes.ResourceExhausted, codes.Unavailable, codes.Aborted:
			t.Logf("ResumeActor %q: %v; retrying", actorName, err)
		default:
			t.Fatalf("ResumeActor %q: %v", actorName, err)
		}
		if actor := getActor(ctx, t, clients, actorName); actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_CRASHED {
			t.Fatalf("Actor %q crashed: %s", actorName, actor.GetStatus().GetCrash().GetMessage())
		}
		if time.Now().After(deadline) {
			t.Fatalf("ResumeActor %q still failing after %v: %v", actorName, timeout, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ResumeActor %q: %v", actorName, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// waitForActorSettled polls until the actor leaves its transitional states.
func waitForActorSettled(ctx context.Context, t *testing.T, clients *e2e.Clients, actorName string, timeout time.Duration) *ateapipb.Actor {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		actor := getActor(ctx, t, clients, actorName)
		switch actor.GetStatus().GetState() {
		case ateapipb.ActorState_ACTOR_STATE_RUNNING, ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ateapipb.ActorState_ACTOR_STATE_CRASHED:
			t.Logf("Actor %q settled in %v", actorName, actor.GetStatus().GetState())
			return actor
		}
		if time.Now().After(deadline) {
			t.Fatalf("Actor %q still %v after %v", actorName, actor.GetStatus().GetState(), timeout)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for Actor %q to settle: %v", actorName, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
