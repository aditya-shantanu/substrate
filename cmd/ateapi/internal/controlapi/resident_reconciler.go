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
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
)

const (
	residentWorkerCount = 5
	// residentResumeParallelism caps the residents one pass restores at once,
	// so filling a pool does not crowd out callers' resumes on the workers.
	residentResumeParallelism = 4
)

// residentPoolStore is the store slice the pool reconciler reads and writes.
type residentPoolStore interface {
	GetActorTemplate(ctx context.Context, templateRef resources.ActorTemplateRef) (*ateapi.ActorTemplate, error)
	ListActorTemplates(ctx context.Context, atespace string, opts store.ListOptions) (store.ListResponse[*ateapi.ActorTemplate], error)
	UpdateActorTemplate(ctx context.Context, templateRef resources.ActorTemplateRef, precondition store.Precondition, mutate func(dbTemplate *ateapi.ActorTemplate) error) (*ateapi.ActorTemplate, error)
	ListActors(ctx context.Context, atespace string, opts store.ListOptions) (store.ListResponse[*ateapi.Actor], error)
	AcquireLease(ctx context.Context, key string) (*store.Lease, error)
}

// residentPoolControl is the in-process slice of the Control service the
// reconciler drives residents through. *RPCService satisfies it.
type residentPoolControl interface {
	CreateResidentActor(ctx context.Context, template *ateapi.ActorTemplate) (*ateapi.Actor, error)
	ResumeActor(ctx context.Context, req *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error)
	DeleteActor(ctx context.Context, req *ateapi.DeleteActorRequest) (*ateapi.Actor, error)
	CreateActorEgressPolicy(ctx context.Context, req *ateapi.CreateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error)
}

// ResidentPoolReconciler keeps each ActorTemplate's resident pool at its
// replicas: Actors restored from the golden snapshot ahead of any claim,
// running under placeholder names until a CreateActor takes one.
type ResidentPoolReconciler struct {
	persistence    residentPoolStore
	control        residentPoolControl
	resyncInterval time.Duration
	queue          workqueue.TypedRateLimitingInterface[resources.ActorTemplateRef]
}

// NewResidentPoolReconciler builds the reconciler over this service: it
// creates and resumes residents through the service's own entry points, and
// CreateActor tells it when a claim takes one, so the pool is topped up at
// once rather than at the next resync.
func (s *RPCService) NewResidentPoolReconciler(persistence residentPoolStore, resyncInterval time.Duration) *ResidentPoolReconciler {
	r := &ResidentPoolReconciler{
		persistence:    persistence,
		control:        s,
		resyncInterval: resyncInterval,
		queue:          workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[resources.ActorTemplateRef]()),
	}
	s.serviceImpl.residentTaken = r.Notify
	return r
}

// Notify asks for a pass over template's pool ahead of the next resync.
func (r *ResidentPoolReconciler) Notify(template resources.ActorTemplateRef) {
	r.queue.Add(template)
}

// Start launches the queue workers and the periodic list that feeds them.
func (r *ResidentPoolReconciler) Start(ctx context.Context) {
	go func() {
		defer r.queue.ShutDown()
		for range residentWorkerCount {
			go wait.UntilWithContext(ctx, r.runWorker, time.Second)
		}
		wait.UntilWithContext(ctx, r.resync, r.resyncInterval)
	}()
}

// resync enqueues every template that wants a pool or still reports one.
func (r *ResidentPoolReconciler) resync(ctx context.Context) {
	pageToken := ""
	for {
		page, err := r.persistence.ListActorTemplates(ctx, "", store.ListOptions{PageSize: templateListPageSize, PageToken: pageToken})
		if err != nil {
			slog.ErrorContext(ctx, "Failed to list actor templates for resident pools", slog.Any("err", err))
			return
		}
		for _, tmpl := range page.Items {
			status := tmpl.GetStatus().GetResidentPoolStatus()
			if tmpl.GetResidentPool().GetReplicas() > 0 || status.GetReady() > 0 || status.GetPending() > 0 {
				r.queue.Add(resources.ActorTemplateRefFromActorTemplate(tmpl))
			}
		}
		if page.NextPageToken == "" {
			return
		}
		pageToken = page.NextPageToken
	}
}

func (r *ResidentPoolReconciler) runWorker(ctx context.Context) {
	for r.processNextWorkItem(ctx) {
	}
}

func (r *ResidentPoolReconciler) processNextWorkItem(ctx context.Context) bool {
	ref, quit := r.queue.Get()
	if quit {
		return false
	}
	defer r.queue.Done(ref)

	if err := r.reconcileOne(ctx, ref); err != nil {
		slog.ErrorContext(ctx, "Failed to reconcile resident pool, requeueing", slog.Any("template", ref), slog.Any("err", err))
		r.queue.AddRateLimited(ref)
		return true
	}
	r.queue.Forget(ref)
	return true
}

// residentPoolLeaseKey serializes passes over one template across replicas.
func residentPoolLeaseKey(ref resources.ActorTemplateRef) string {
	return "lease:residentpool:" + ref.Atespace + ":" + ref.Name
}

// reconcileOne brings one template's pool to its replicas: broken members
// are deleted, idle ones resumed, missing ones created and resumed, extra ones
// deleted, and the counts written to the template's status.
func (r *ResidentPoolReconciler) reconcileOne(ctx context.Context, ref resources.ActorTemplateRef) error {
	lease, err := r.persistence.AcquireLease(ctx, residentPoolLeaseKey(ref))
	if err != nil {
		if errors.Is(err, store.ErrLeaseConflict) {
			return nil
		}
		return fmt.Errorf("while acquiring lease: %w", err)
	}
	defer lease.Close()
	ctx = lease.Context()

	tmpl, err := r.persistence.GetActorTemplate(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		// DeleteActorTemplate deletes the residents under the template lease.
		return nil
	}
	if err != nil {
		return err
	}
	if tmpl.GetMetadata().GetDeleteTime() != nil {
		return nil
	}

	want := int(tmpl.GetResidentPool().GetReplicas())
	if want > 0 && hasExternalVolumes(tmpl) {
		// Volumes are created per Actor at its first resume, so a sandbox
		// restored ahead of the claim cannot carry the claimant's.
		want = 0
		if err := r.writeStatus(ctx, tmpl, 0, 0, "resident pools do not support ActorTemplates with external volumes"); err != nil {
			return err
		}
	}

	var ready, idle, broken []*ateapi.Actor
	pending := 0
	if err := forEachResident(ctx, r.persistence, tmpl, func(actor *ateapi.Actor) {
		if actor.GetStatus().GetResident().GetState() != ateapi.ResidentState_RESIDENT_STATE_POOLED || actor.GetMetadata().GetDeleteTime() != nil {
			return
		}
		switch actor.GetStatus().GetState() {
		case ateapi.ActorState_ACTOR_STATE_RUNNING:
			ready = append(ready, actor)
		case ateapi.ActorState_ACTOR_STATE_RESUMING:
			pending++
		case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
			// Created but not resumed yet, or suspended by a drain.
			idle = append(idle, actor)
		default:
			// CRASHED, SUSPENDING, REVERTING: not worth keeping.
			broken = append(broken, actor)
		}
	}); err != nil {
		return err
	}

	var errs []error
	for _, actor := range broken {
		if err := r.deleteResident(ctx, actor); err != nil {
			errs = append(errs, err)
		}
	}

	// Extra members go first, idle ones before running ones.
	have := len(ready) + len(idle) + pending
	for have > want && len(idle) > 0 {
		if err := r.deleteResident(ctx, idle[len(idle)-1]); err != nil {
			errs = append(errs, err)
		}
		idle = idle[:len(idle)-1]
		have--
	}
	for have > want && len(ready) > 0 {
		if err := r.deleteResident(ctx, ready[len(ready)-1]); err != nil {
			errs = append(errs, err)
		}
		ready = ready[:len(ready)-1]
		have--
	}

	// Missing members: create, then resume the idle set in parallel.
	toResume := idle
	for have < want {
		actor, err := r.control.CreateResidentActor(ctx, tmpl)
		if err != nil {
			errs = append(errs, fmt.Errorf("while creating a resident: %w", err))
			break
		}
		toResume = append(toResume, actor)
		have++
	}
	resumed, resumeErrs := r.resumeResidents(ctx, tmpl, toResume)
	errs = append(errs, resumeErrs...)
	readyCount := len(ready) + resumed
	pendingCount := pending + len(toResume) - resumed

	msg := ""
	if err := errors.Join(errs...); err != nil {
		msg = truncateUTF8(err.Error(), maxGoldenErrorMessageLen)
	}
	if err := r.writeStatus(ctx, tmpl, readyCount, pendingCount, msg); err != nil {
		return err
	}
	return errors.Join(errs...)
}

// resumeResidents resumes actors, at most residentResumeParallelism at a
// time, and returns how many came up RUNNING.
func (r *ResidentPoolReconciler) resumeResidents(ctx context.Context, tmpl *ateapi.ActorTemplate, actors []*ateapi.Actor) (int, []error) {
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		sem     = make(chan struct{}, residentResumeParallelism)
		resumed int
		errs    []error
	)
	for _, actor := range actors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := r.resumeResident(ctx, tmpl, actor)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			resumed++
		}()
	}
	wg.Wait()
	return resumed, errs
}

// resumeResident gives a resident the template's golden egress policy, as the
// golden actor gets it, and resumes it.
func (r *ResidentPoolReconciler) resumeResident(ctx context.Context, tmpl *ateapi.ActorTemplate, actor *ateapi.Actor) error {
	ref := resources.ActorRefFromActor(actor).ToObjectRef()
	if want := tmpl.GetGoldenEgressPolicy(); want != nil {
		_, err := r.control.CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{
			Actor: ref,
			EgressPolicy: &ateapi.EgressPolicy{
				Metadata: &ateapi.ResourceMetadata{Atespace: ref.GetAtespace(), Name: resources.SingletonName},
				Rules:    proto.CloneOf(want).GetRules(),
			},
		})
		if err != nil && apierror.Code(err) != codes.AlreadyExists {
			return fmt.Errorf("while creating the egress policy of resident %s: %w", ref.GetName(), err)
		}
	}
	resp, err := r.control.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref})
	if err != nil {
		return fmt.Errorf("while resuming resident %s: %w", ref.GetName(), err)
	}
	if st := resp.GetActor().GetStatus().GetState(); st != ateapi.ActorState_ACTOR_STATE_RUNNING {
		return fmt.Errorf("resident %s is %s after resume", ref.GetName(), st)
	}
	return nil
}

func (r *ResidentPoolReconciler) deleteResident(ctx context.Context, actor *ateapi.Actor) error {
	ref := resources.ActorRefFromActor(actor)
	if _, err := r.control.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref.ToObjectRef()}); err != nil && apierror.Code(err) != codes.NotFound {
		return fmt.Errorf("while deleting resident %s: %w", ref.Name, err)
	}
	return nil
}

// writeStatus records the pool's counts on the template. A concurrent template
// write makes the pass retry.
func (r *ResidentPoolReconciler) writeStatus(ctx context.Context, tmpl *ateapi.ActorTemplate, ready, pending int, errMsg string) error {
	ref := resources.ActorTemplateRefFromActorTemplate(tmpl)
	latest, err := r.persistence.GetActorTemplate(ctx, ref)
	if err != nil {
		return err
	}
	want := &ateapi.ResidentPoolStatus{Ready: int32(ready), Pending: int32(pending), ErrorMessage: errMsg}
	if proto.Equal(latest.GetStatus().GetResidentPoolStatus(), want) {
		return nil
	}
	_, err = r.persistence.UpdateActorTemplate(ctx, ref, store.PreconditionFrom(latest), func(dbTemplate *ateapi.ActorTemplate) error {
		if dbTemplate.Status == nil {
			dbTemplate.Status = &ateapi.ActorTemplateStatus{}
		}
		dbTemplate.Status.ResidentPoolStatus = want
		return nil
	})
	if err != nil {
		return fmt.Errorf("while writing the resident pool status of %s: %w", ref, err)
	}
	return nil
}

// hasExternalVolumes reports whether tmpl declares CSI-backed volumes.
func hasExternalVolumes(tmpl *ateapi.ActorTemplate) bool {
	for _, volume := range tmpl.GetVolumes() {
		if volume.GetExternal() != nil {
			return true
		}
	}
	return false
}
