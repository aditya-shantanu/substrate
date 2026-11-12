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

package controlapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/apivalidation"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/cmd/ateapi/internal/store/storetest"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/grpc/codes"
)

// seedReservedActor stores an actor a CreateActor built over a pooled
// resident: SUSPENDED, bound to the wire test worker, RESERVED from pooledName.
func seedReservedActor(t *testing.T, ctx context.Context, st store.Interface, ref resources.ActorRef, pooledName string) *ateapi.Actor {
	t.Helper()
	seedWorkflowActor(t, ctx, st, ref, "ns", "tmpl1", ateapi.ActorState_ACTOR_STATE_SUSPENDED, func(a *ateapi.Actor) {
		a.Status.WorkerAssignment = wireTestAssignment()
		a.Status.AssignedNode = wireTestAssignment().GetNodeName()
		a.Status.Resident = &ateapi.ResidentStatus{State: ateapi.ResidentState_RESIDENT_STATE_RESERVED, PooledName: pooledName}
	})
	actor := mustReadActor(t, ctx, st, ref)
	seedWireWorker(t, ctx, st, assignmentOf(actor))
	worker, err := st.GetWorker(ctx, wireTestAssignment().GetWorker().GetName())
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if _, err := st.UpdateWorker(ctx, worker.GetMetadata().GetName(), store.PreconditionFrom(worker), func(w *ateapi.Worker) error {
		w.Status = registered(w.Status)
		return nil
	}); err != nil {
		t.Fatalf("UpdateWorker: %v", err)
	}
	return actor
}

// TestResumeActor_AdoptsReservedResident pins the reserved path: the first
// resume of an actor that took a pooled resident sends atelet one Adopt
// naming the placeholder and the new name, no Restore, and commits RUNNING
// with the resident ADOPTED.
func TestResumeActor_AdoptsReservedResident(t *testing.T) {
	ctx := context.Background()
	st := newTestPersistence(t)
	newTestActorWorkflow(t, st, "ns", "tmpl1")
	storetest.MustCreateAtespace(t, ctx, st, "team-a")
	w, atelet := newWireCaptureWorkflow(t, st)

	ref := resources.ActorRef{Atespace: "team-a", Name: "id1"}
	seedReservedActor(t, ctx, st, ref, "rsd-abcd1234-xyz")

	got, resumed, err := w.ResumeActor(ctx, ref)
	if err != nil {
		t.Fatalf("ResumeActor: %v", err)
	}
	if !resumed {
		t.Errorf("resumed = false, want true")
	}
	if atelet.restore != nil || atelet.run != nil {
		t.Errorf("atelet got Restore %v / Run %v, want neither", atelet.restore, atelet.run)
	}
	req := atelet.adopt
	if req == nil {
		t.Fatal("atelet got no Adopt")
	}
	if req.GetPreviousActorName() != "rsd-abcd1234-xyz" || req.GetActorName() != "id1" || req.GetAtespace() != "team-a" ||
		req.GetWorkerPodUid() != wireTestAssignment().GetWorkerPodUid() || req.GetActorTemplateName() != "tmpl1" {
		t.Errorf("Adopt request = %v, want rsd-abcd1234-xyz -> team-a/id1 on %s from tmpl1", req, wireTestAssignment().GetWorkerPodUid())
	}
	status := got.GetStatus()
	if status.GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING ||
		status.GetResident().GetState() != ateapi.ResidentState_RESIDENT_STATE_ADOPTED ||
		status.GetResident().GetPooledName() != "rsd-abcd1234-xyz" ||
		status.GetWorkerAssignment().GetWorker().GetName() != wireTestAssignment().GetWorker().GetName() {
		t.Errorf("status = %v, want RUNNING, ADOPTED from rsd-abcd1234-xyz, still on the resident's worker", status)
	}
	if stored := mustReadActor(t, ctx, st, ref); stored.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("stored state = %v, want RUNNING", stored.GetStatus().GetState())
	}
}

// TestResumeActor_ReleasesResidentWhenAdoptFails pins the fallback: when
// atelet cannot adopt (an older worker says Unimplemented), the
// resident is terminated and the actor returns to plain SUSPENDED with no
// worker, so the resume continues as an ordinary restore.
func TestResumeActor_ReleasesResidentWhenAdoptFails(t *testing.T) {
	ctx := context.Background()
	st := newTestPersistence(t)
	newTestActorWorkflow(t, st, "ns", "tmpl1")
	storetest.MustCreateAtespace(t, ctx, st, "team-a")
	w, atelet := newWireCaptureWorkflow(t, st)
	atelet.adoptErr = apierror.Unimplemented("worker has no AdoptWorkload")
	terminated := false
	atelet.onTerminate = func() { terminated = true }

	ref := resources.ActorRef{Atespace: "team-a", Name: "id1"}
	actor := seedReservedActor(t, ctx, st, ref, "rsd-abcd1234-xyz")

	// Without a scheduler the ordinary resume that follows the release cannot
	// place the actor; the release itself is what this test checks.
	_, err := w.resumeReserved(ctx, ref, actor, &ateapi.ActorTemplate{Metadata: &ateapi.ResourceMetadata{Atespace: "ns", Name: "tmpl1"}})
	if !errors.Is(err, errResidentUnusable) {
		t.Fatalf("resumeReserved err = %v, want errResidentUnusable", err)
	}
	if !terminated {
		t.Error("atelet got no Terminate for the resident")
	}
	if got := atelet.terminate.GetActorName(); got != "rsd-abcd1234-xyz" {
		t.Errorf("Terminate named actor %q, want the placeholder rsd-abcd1234-xyz the sandbox still runs under", got)
	}
	stored := mustReadActor(t, ctx, st, ref)
	status := stored.GetStatus()
	if status.GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED || status.GetWorkerAssignment() != nil ||
		status.GetAssignedNode() != "" || status.GetResident() != nil {
		t.Errorf("status after release = %v, want SUSPENDED with no worker, node or resident", status)
	}
	if _, err := st.GetWorkerAssignment(ctx, wireTestAssignment().GetWorker().GetName(), actor.GetMetadata().GetUid()); apierror.Code(err) == codes.OK {
		t.Error("the resident's assignment row survived the release")
	}
}

// TestCreateResidentActor pins the row the pool reconciler stores: a valid
// placeholder name under the template's uid prefix, SUSPENDED, POOLED.
func TestCreateResidentActor(t *testing.T) {
	ctx := context.Background()
	st := newTestPersistence(t)
	newTestActorWorkflow(t, st, "ns", "tmpl1")
	tmpl, err := st.GetActorTemplate(ctx, resources.ActorTemplateRef{Atespace: "ns", Name: "tmpl1"})
	if err != nil {
		t.Fatalf("GetActorTemplate: %v", err)
	}
	s := newServiceImpl(st, nil)

	actor, err := s.CreateResidentActor(ctx, tmpl)
	if err != nil {
		t.Fatalf("CreateResidentActor: %v", err)
	}
	name := actor.GetMetadata().GetName()
	wantPrefix := residentPrefixFor(tmpl)
	if !strings.HasPrefix(name, wantPrefix) || len(name) != len(wantPrefix)+8 || strings.ToLower(name) != name {
		t.Errorf("resident name = %q, want %s plus eight lower-case characters", name, wantPrefix)
	}
	if errs := apivalidation.ValidateCreateActorRequest(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Atespace: "ns", Name: name}, ActorTemplate: actor.GetActorTemplate()}}); len(errs) > 0 {
		t.Errorf("resident name %q fails validation: %v", name, errs)
	}
	if st := actor.GetStatus(); st.GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED || st.GetResident().GetState() != ateapi.ResidentState_RESIDENT_STATE_POOLED {
		t.Errorf("status = %v, want SUSPENDED and POOLED", st)
	}
	pooled, err := listPooledResidents(ctx, st, tmpl)
	if err != nil {
		t.Fatalf("listPooledResidents: %v", err)
	}
	// Not RUNNING yet, so not claimable.
	if len(pooled) != 0 {
		t.Errorf("listPooledResidents = %d residents, want 0 before the resident runs", len(pooled))
	}
}
