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
)

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

// resumeActorRetrying resumes the actor and waits until it is RUNNING. On
// top of ResumeActorAwaitCapacity it retries Aborted, while a suspend the
// client abandoned still holds the actor's lease, and Unavailable.
func resumeActorRetrying(ctx context.Context, t *testing.T, clients *e2e.Clients, actorName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		_, err := e2e.ResumeActorAwaitCapacity(t, ctx, clients, &ateapipb.ResumeActorRequest{
			Actor: &ateapipb.ObjectRef{Atespace: demoAtespace, Name: actorName},
		})
		if err == nil {
			waitForActorState(ctx, t, clients, actorName, ateapipb.ActorState_ACTOR_STATE_RUNNING)
			return
		}
		if code := status.Code(err); (code != codes.Aborted && code != codes.Unavailable) || time.Now().After(deadline) {
			t.Fatalf("ResumeActor %q: %v", actorName, err)
		}
		t.Logf("ResumeActor %q: %v; retrying", actorName, err)
		select {
		case <-ctx.Done():
			t.Fatalf("ResumeActor %q: %v", actorName, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
