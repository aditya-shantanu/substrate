// Copyright 2026 The Agent Substrate Authors
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
	"testing"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/e2e"
	"go.ate.dev/substrate/internal/resources"
	"k8s.io/apimachinery/pkg/util/wait"
)

// TestResidentPoolLifecycle checks a template's resident pool end to end: the
// pool fills, a new actor takes a resident and its first resume adopts the
// running sandbox, the pool refills, and deleting the template removes what
// is left of it.
func TestResidentPoolLifecycle(t *testing.T) {
	if e2e.IsMicroVM() {
		t.Skip("ateworker-microvm does not adopt yet; the resume falls back to a restore")
	}
	ctx := t.Context()
	clients := e2e.GetClients()
	ns := e2e.CreateNamespace(t)
	atespace := "resident-" + ns.Name
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = clients.SubstrateAPI.DeleteAtespace(cleanupCtx, &ateapi.DeleteAtespaceRequest{Atespace: &ateapi.ObjectRef{Name: atespace}})
	})
	const replicas = 2
	created := e2e.CreateSubstrateCounterTemplate(ctx, t, clients, ns.Name, e2e.SubstrateTemplateOptions{
		Atespace: atespace, Name: "counter", PoolName: "counter", PoolReplicas: 1,
		Labels:         map[string]string{"resident-e2e": ns.Name},
		SnapshotConfig: templateSnapshotConfig(t, ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, atespace),
		Modify: func(tmpl *ateapi.ActorTemplate) {
			tmpl.ResidentPool = &ateapi.ResidentPool{Replicas: replicas}
		},
	})
	templateRef := e2e.TemplateRef(created)

	// The pool fills once the golden exists.
	waitForResidents(t, ctx, clients, templateRef, replicas)
	residents := listResidents(t, ctx, clients, atespace)
	if len(residents) != replicas {
		t.Fatalf("pooled residents = %v, want %d", residents, replicas)
	}

	actorRef := &ateapi.ObjectRef{Atespace: atespace, Name: "claim"}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = clients.SubstrateAPI.DeleteActor(cleanupCtx, &ateapi.DeleteActorRequest{Actor: actorRef})
	})
	actor, err := clients.SubstrateAPI.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Atespace: atespace, Name: actorRef.Name}, ActorTemplate: templateRef,
	}})
	if err != nil {
		t.Fatal(err)
	}
	status := actor.GetStatus()
	if status.GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED ||
		status.GetResident().GetState() != ateapi.ResidentState_RESIDENT_STATE_RESERVED ||
		status.GetWorkerAssignment() == nil {
		t.Fatalf("new actor status = %v, want SUSPENDED, RESERVED, with the resident's worker", status)
	}
	pooledName := status.GetResident().GetPooledName()
	if !strings.HasPrefix(pooledName, "rsd-") {
		t.Fatalf("pooled_name = %q, want a resident name", pooledName)
	}
	if _, err := clients.SubstrateAPI.GetActor(ctx, &ateapi.GetActorRequest{Actor: &ateapi.ObjectRef{Atespace: atespace, Name: pooledName}}); err == nil {
		t.Fatalf("resident %s still exists after it was taken", pooledName)
	}

	resumed, err := e2e.ResumeActorAwaitCapacity(t, ctx, clients, &ateapi.ResumeActorRequest{Actor: actorRef})
	if err != nil {
		t.Fatal(err)
	}
	status = resumed.GetActor().GetStatus()
	if status.GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING ||
		status.GetResident().GetState() != ateapi.ResidentState_RESIDENT_STATE_ADOPTED {
		t.Fatalf("resumed status = %v, want RUNNING and ADOPTED", status)
	}
	if status.GetWorkerAssignment().GetWorker().GetName() != actor.GetStatus().GetWorkerAssignment().GetWorker().GetName() {
		t.Fatalf("resume moved the actor to worker %s from the resident's %s",
			status.GetWorkerAssignment().GetWorker().GetName(), actor.GetStatus().GetWorkerAssignment().GetWorker().GetName())
	}
	// The router reaches the adopted sandbox under the new name, and the
	// counter's memory is the golden's: this is its first request.
	response, err := callActor(t, resources.ActorRefFromObjectRef(actorRef))
	if err != nil {
		t.Fatal(err)
	}
	validateCounterResponse(t, response, "adopted resident", 1, 1)

	// The pool is topped up behind the claim.
	waitForResidents(t, ctx, clients, templateRef, replicas)

	if _, err := clients.SubstrateAPI.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: actorRef}); err != nil {
		t.Fatal(err)
	}
	if _, err := clients.SubstrateAPI.DeleteActorTemplate(ctx, &ateapi.DeleteActorTemplateRequest{ActorTemplate: templateRef}); err != nil {
		t.Fatal(err)
	}
	if left := listResidents(t, ctx, clients, atespace); len(left) != 0 {
		t.Fatalf("residents left after deleting the template: %v", left)
	}
}

// waitForResidents polls until the template reports want ready residents.
func waitForResidents(t *testing.T, ctx context.Context, clients *e2e.Clients, templateRef *ateapi.ObjectRef, want int32) {
	t.Helper()
	var last *ateapi.ResidentPoolStatus
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, e2e.TemplateReadyTimeout(t), true, func(ctx context.Context) (bool, error) {
		tmpl, err := clients.SubstrateAPI.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{ActorTemplate: templateRef})
		if err != nil {
			return false, err
		}
		last = tmpl.GetStatus().GetResidentPoolStatus()
		return last.GetReady() == want, nil
	})
	if err != nil {
		t.Fatalf("waiting for %d ready residents: %v (last status %v)", want, err, last)
	}
}

// listResidents returns the names of the pooled residents in atespace.
func listResidents(t *testing.T, ctx context.Context, clients *e2e.Clients, atespace string) []string {
	t.Helper()
	var names []string
	for token := ""; ; {
		resp, err := clients.SubstrateAPI.ListActors(ctx, &ateapi.ListActorsRequest{Atespace: atespace, Options: &ateapi.ListOptions{PageToken: token}})
		if err != nil {
			t.Fatal(err)
		}
		for _, actor := range resp.GetActors() {
			if actor.GetStatus().GetResident().GetState() == ateapi.ResidentState_RESIDENT_STATE_POOLED {
				names = append(names, actor.GetMetadata().GetName())
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			return names
		}
	}
}
