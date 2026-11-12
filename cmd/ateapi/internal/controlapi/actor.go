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
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/apivalidation"
	"go.ate.dev/substrate/cmd/ateapi/internal/defaults"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/actoridjwt"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/ateattr"
	"go.ate.dev/substrate/internal/preview"
	"go.ate.dev/substrate/internal/resources"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func (s *RPCService) CreateActor(ctx context.Context, req *ateapi.CreateActorRequest) (created *ateapi.Actor, err error) {
	// First scrub any fields that users are not allowed to set, then fill the
	// defaults so validation sees the final resource state.
	inActor := req.Actor
	if inActor != nil { // otherwise validation will flag it
		scrubResourceMetadataForCreate(inActor.Metadata)
		inActor.Status = nil
		defaults.Apply(inActor)
	}

	// Validate the request, including the object within it.
	if errs := apivalidation.ValidateCreateActorRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}

	start := time.Now()
	// Recorded only after validation, so every operation uniformly measures a
	// validated request; malformed ones stay visible in rpc.server.call.duration.
	defer func() {
		s.instruments.recordLifecycleOp(ctx, ateattr.OperationCreate, start, err,
			ateattr.TemplateNameKey.String(inActor.GetActorTemplate().GetName()),
			ateattr.TemplateAtespaceKey.String(inActor.GetActorTemplate().GetAtespace()),
		)
	}()

	setSpanActorRefAttributes(ctx, resources.ActorRefFromActor(inActor))

	// Handle the creation, including validation of the final stored object.
	stored, err := s.impl.CreateActor(ctx, inActor)
	setSpanActorAttributes(ctx, stored)

	return stored, err
}

func (s *ServiceImpl) CreateActor(ctx context.Context, inActor *ateapi.Actor) (*ateapi.Actor, error) {
	template, outActor, err := s.newActorFromTemplate(ctx, inActor)
	if err != nil {
		return nil, err
	}
	atespace := inActor.GetMetadata().GetAtespace()
	name := inActor.GetMetadata().GetName()

	// A pooled resident of the template takes the place of a fresh row: the
	// Actor is born SUSPENDED like any other, but its first resume adopts the
	// resident's running sandbox instead of restoring the golden snapshot.
	if inActor.GetSourceTag() == nil && template.GetResidentPool().GetReplicas() > 0 {
		stored, err := s.adoptResident(ctx, template, outActor)
		if err != nil {
			return nil, err
		}
		if stored != nil {
			logActorStateChanged(ctx, stored, ateattr.OperationCreate)
			return stored, nil
		}
	}

	// Save the data in the storage layer.
	stored, err := s.store.CreateActor(ctx, outActor)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil, apierror.AlreadyExists("Actor %s already exists", name)
		}
		if errors.Is(err, store.ErrFailedPrecondition) {
			return nil, apierror.FailedPrecondition("Atespace %s not found", atespace)
		}
		return nil, fmt.Errorf("while recording actor: %w", err)
	}

	// Without this an actor that is created and never resumed has no record at
	// all, at any retention.
	logActorStateChanged(ctx, stored, ateattr.OperationCreate)

	return stored, nil
}

// newActorFromTemplate resolves inActor's template and source tag and returns
// the validated Actor to store: SUSPENDED, with the tag's snapshot borrowed
// when there is one.
func (s *ServiceImpl) newActorFromTemplate(ctx context.Context, inActor *ateapi.Actor) (*ateapi.ActorTemplate, *ateapi.Actor, error) {
	// Check that the referenced ActorTemplate exists.
	// FIXME: This is not atomic and it is not a guarantee that the template
	// will still exist later.  Checking it here produces a nice error UX, but
	// we still have to handle the template not existing later, which makes the
	// UX inconsistent, at best.  Is it actually worth checking at all?
	template, err := resolveActorTemplate(ctx, s.store, inActor)
	if err != nil {
		return nil, nil, err
	}

	// Resolve the explicit tag, or freeze the template's current golden default.
	tagRef := inActor.GetSourceTag()
	if tagRef == nil {
		tagRef = template.GetStatus().GetGoldenSnapshotStatus().GetGoldenTag()
	} else {
		// NOTE: whether the ExternalVolumes gate is enabled or not, we do not
		// support cloning from a tag that has external volumes.
		for _, volume := range template.GetVolumes() {
			if volume.GetExternal().GetTemplate() != nil {
				// TODO: Permit cloning after CSI volume snapshots are supported.
				return nil, nil, apierror.FailedPrecondition("Tag cloning does not support ActorTemplates with external volumes")
			}
		}
	}
	var sourceTag *ateapi.Tag
	if tagRef != nil {
		sourceTag, err = s.resolveTagSource(ctx, inActor.GetMetadata().GetAtespace(), tagRef, template)
		if err != nil {
			return nil, nil, err
		}
	}

	// Volume creation is completed asynchronously after the actor is recorded.
	var initVols []*ateapi.ExternalVolumeStatus
	if iv, err := initialExternalVolumes(ctx, s.storageClassLister, template); err != nil {
		return nil, nil, err
	} else if preview.IsEnabledFor(ctx, preview.GateExternalVolumes) {
		initVols = iv
	} else if len(iv) > 0 {
		errs := field.ErrorList{
			field.Forbidden(field.NewPath("actor", "actor_template"),
				"ActorTemplates with external volumes require the ExternalVolumes preview gate to be enabled on the server and the client to opt in to preview features (kubectl ate --preview, or the "+preview.Header+" header)"),
		}
		return nil, nil, resources.ToAPIError(errs)
	}

	// Verify that the result is properly valid before storing it.
	outActor := proto.CloneOf(inActor)
	outActor.Status = &ateapi.ActorStatus{
		State:           ateapi.ActorState_ACTOR_STATE_SUSPENDED,
		ExternalVolumes: externalVolumesStatus(initVols),
	}
	if sourceTag != nil {
		// The Actor starts out borrowing the tag's snapshot rather than
		// copying it. The snapshot URI is under the tag's prefix, not the Actor's, which
		// is what keeps the Actor from collecting those objects. Its first
		// suspend writes a snapshot under its own prefix and takes over from
		// there.
		snap := proto.CloneOf(sourceTag.GetStatus().GetSnapshot())
		snap.Generation = 1
		snap.Owner = ateapi.SnapshotOwner_SNAPSHOT_OWNER_TAG
		// The Actor is born with guest state, so stamp the template that state
		// was built on now rather than at the first resume.
		snap.ActorTemplateUid = sourceTag.GetStatus().GetActorTemplateUid()
		outActor.Status.Snapshots = []*ateapi.Snapshot{snap}
		outActor.Status.LastAssignedGeneration = 1
	}
	if errs := apivalidation.ValidateActorUpdate(ctx, field.NewPath("actor"), outActor, inActor, true); len(errs) > 0 {
		return nil, nil, resources.ToInternalError(errs)
	}
	return template, outActor, nil
}

// residentNamePrefix starts the placeholder name of every pooled resident.
const residentNamePrefix = "rsd-"

// adoptResident stores outActor over one of template's pooled residents and
// returns it, or nil when the pool has none to give. A resident another
// CreateActor took first is skipped.
func (s *ServiceImpl) adoptResident(ctx context.Context, template *ateapi.ActorTemplate, outActor *ateapi.Actor) (*ateapi.Actor, error) {
	residents, err := listPooledResidents(ctx, s.store, template)
	if err != nil {
		return nil, err
	}
	for _, resident := range residents {
		stored, err := s.store.AdoptResidentActor(ctx, resources.ActorRefFromActor(resident), resident.GetMetadata().GetUid(), outActor)
		switch {
		case err == nil:
			slog.InfoContext(ctx, "Actor took a pooled resident",
				slog.String("actor", resources.ActorRefFromActor(stored).String()),
				slog.String("resident", resident.GetMetadata().GetName()),
				slog.String("worker", stored.GetStatus().GetWorkerAssignment().GetWorker().GetName()))
			return stored, nil
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrUIDConflict), errors.Is(err, store.ErrFailedPrecondition):
			// Taken, deleted or no longer pooled since the list.
			continue
		case errors.Is(err, store.ErrAlreadyExists):
			return nil, apierror.AlreadyExists("Actor %s already exists", outActor.GetMetadata().GetName())
		default:
			return nil, fmt.Errorf("while adopting resident %s: %w", resident.GetMetadata().GetName(), err)
		}
	}
	return nil, nil
}

// listPooledResidents returns template's residents that are running and
// unclaimed, in the template's atespace.
func listPooledResidents(ctx context.Context, lister actorLister, template *ateapi.ActorTemplate) ([]*ateapi.Actor, error) {
	var pooled []*ateapi.Actor
	err := forEachResident(ctx, lister, template, func(actor *ateapi.Actor) {
		if actor.GetStatus().GetResident().GetState() == ateapi.ResidentState_RESIDENT_STATE_POOLED &&
			actor.GetStatus().GetState() == ateapi.ActorState_ACTOR_STATE_RUNNING &&
			actor.GetMetadata().GetDeleteTime() == nil {
			pooled = append(pooled, actor)
		}
	})
	return pooled, err
}

type actorLister interface {
	ListActors(ctx context.Context, atespace string, opts store.ListOptions) (store.ListResponse[*ateapi.Actor], error)
}

// forEachResident calls fn for every Actor in template's atespace that the
// resident pool created for this template. Residents keep the template's uid
// in their name, so a recreated template of the same name never adopts the
// old pool.
func forEachResident(ctx context.Context, lister actorLister, template *ateapi.ActorTemplate, fn func(*ateapi.Actor)) error {
	prefix := residentPrefixFor(template)
	templateRef := resources.ActorTemplateRefFromActorTemplate(template)
	for token := ""; ; {
		page, err := lister.ListActors(ctx, template.GetMetadata().GetAtespace(), store.ListOptions{PageSize: maxPageSize, PageToken: token})
		if err != nil {
			return fmt.Errorf("while listing residents of %s: %w", templateRef, err)
		}
		for _, actor := range page.Items {
			if !strings.HasPrefix(actor.GetMetadata().GetName(), prefix) || actor.GetStatus().GetResident() == nil {
				continue
			}
			if resources.ActorTemplateRefFromObjectRef(actor.GetActorTemplate()) != templateRef {
				continue
			}
			fn(actor)
		}
		if page.NextPageToken == "" {
			return nil
		}
		token = page.NextPageToken
	}
}

// residentPrefixFor is the name prefix of template's residents: "rsd-" and
// the first eight characters of the template's uid.
func residentPrefixFor(template *ateapi.ActorTemplate) string {
	uid := template.GetMetadata().GetUid()
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return residentNamePrefix + uid + "-"
}

// CreateResidentActor stores a new pooled resident of template: an Actor
// under a placeholder name that the pool reconciler resumes and CreateActor
// later hands to a caller. It is SUSPENDED at the golden snapshot like any new
// Actor, marked POOLED.
func (s *ServiceImpl) CreateResidentActor(ctx context.Context, template *ateapi.ActorTemplate) (*ateapi.Actor, error) {
	inActor := &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{
			Atespace: template.GetMetadata().GetAtespace(),
			Name:     residentPrefixFor(template) + strings.ToLower(rand.Text()[:8]),
		},
		ActorTemplate: resources.ActorTemplateRefFromActorTemplate(template).ToObjectRef(),
	}
	defaults.Apply(inActor)
	if errs := apivalidation.ValidateCreateActorRequest(ctx, &ateapi.CreateActorRequest{Actor: inActor}); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}
	_, outActor, err := s.newActorFromTemplate(ctx, inActor)
	if err != nil {
		return nil, err
	}
	outActor.Status.Resident = &ateapi.ResidentStatus{State: ateapi.ResidentState_RESIDENT_STATE_POOLED}
	stored, err := s.store.CreateActor(ctx, outActor)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil, apierror.AlreadyExists("Actor %s already exists", inActor.GetMetadata().GetName())
		}
		if errors.Is(err, store.ErrFailedPrecondition) {
			return nil, apierror.FailedPrecondition("Atespace %s not found", inActor.GetMetadata().GetAtespace())
		}
		return nil, fmt.Errorf("while recording resident actor: %w", err)
	}
	logActorStateChanged(ctx, stored, ateattr.OperationCreate)
	return stored, nil
}

// resolveTagSource resolves a CreateActor request's source tag
// and checks that the tag is usable for creating an Actor in actorAtespace
// from template.
func (s *ServiceImpl) resolveTagSource(ctx context.Context, actorAtespace string, tagRef *ateapi.ObjectRef, template *ateapi.ActorTemplate) (*ateapi.Tag, error) {
	tag, err := s.store.GetTag(ctx, resources.TagRefFromObjectRef(tagRef))
	if errors.Is(err, store.ErrNotFound) {
		return nil, apierror.FailedPrecondition("Tag not found")
	}
	if err != nil {
		return nil, fmt.Errorf("while getting tag: %w", err)
	}
	switch tag.GetScope() {
	case ateapi.TagScope_TAG_SCOPE_ATESPACE:
		if tag.GetMetadata().GetAtespace() != actorAtespace {
			return nil, apierror.FailedPrecondition("Tag is not published outside its Atespace")
		}
	case ateapi.TagScope_TAG_SCOPE_PUBLISHED:
	default:
		return nil, apierror.FailedPrecondition("source Tag has an invalid scope")
	}
	// DeleteTag may already have collected the snapshot of a tag with
	// delete_time set.
	if tag.GetMetadata().GetDeleteTime() != nil {
		return nil, apierror.FailedPrecondition("source Tag %s/%s is being deleted", tagRef.GetAtespace(), tagRef.GetName())
	}
	// A tag might have an empty Snapshot URI if the tag creation failed or is ongoing.
	if tagDurableSnapshotURI(tag, ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED) == "" {
		return nil, apierror.FailedPrecondition("source Tag is still being created or failed creation")
	}
	// TODO: Permit compatible VOLUMES snapshots when runtimes can extract portable data.
	if tag.GetStatus().GetActorTemplateUid() != template.GetMetadata().GetUid() {
		return nil, apierror.FailedPrecondition("source Tag must be taken from an actor with ActorTemplate uid %q", tag.GetStatus().GetActorTemplateUid())
	}
	return tag, nil
}

func (s *RPCService) GetActor(ctx context.Context, req *ateapi.GetActorRequest) (*ateapi.Actor, error) {
	if errs := apivalidation.ValidateGetActorRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}
	actorRef := resources.ActorRefFromObjectRef(req.GetActor())
	actor, err := s.impl.GetActor(ctx, actorRef)
	if errors.Is(err, store.ErrNotFound) {
		return nil, apierror.NotFound("Actor %s not found", actorRef)
	} else if err != nil {
		return nil, fmt.Errorf("while getting actor from DB: %w", err)
	}
	return actor, nil
}

func (s *ServiceImpl) GetActor(ctx context.Context, actorRef resources.ActorRef) (*ateapi.Actor, error) {
	return s.store.GetActor(ctx, actorRef)
}

// CreateResidentActor is the in-process entry the resident pool reconciler
// creates pool members through. It is not a Control RPC.
func (s *RPCService) CreateResidentActor(ctx context.Context, template *ateapi.ActorTemplate) (*ateapi.Actor, error) {
	return s.serviceImpl.CreateResidentActor(ctx, template)
}

func (s *RPCService) ListActors(ctx context.Context, req *ateapi.ListActorsRequest) (*ateapi.ListActorsResponse, error) {
	if errs := apivalidation.ValidateListActorsRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}

	page, err := s.impl.ListActors(ctx, req.GetAtespace(), toStoreListOptions(req.GetOptions()))
	if err != nil {
		return nil, mapListError(fmt.Errorf("while listing actors in db: %w", err))
	}
	return &ateapi.ListActorsResponse{
		Actors:        page.Items,
		NextPageToken: page.NextPageToken,
	}, nil
}

func (s *ServiceImpl) ListActors(ctx context.Context, atespace string, opts store.ListOptions) (store.ListResponse[*ateapi.Actor], error) {
	return s.store.ListActors(ctx, atespace, opts)
}

func (s *RPCService) UpdateActor(ctx context.Context, req *ateapi.UpdateActorRequest) (*ateapi.Actor, error) {
	// First scrub any fields that users are not allowed to set.
	inActor := req.Actor
	if inActor != nil { // otherwise validation will flag it
		scrubResourceMetadataForUpdate(inActor.Metadata)
		inActor.Status = nil
	}

	// Validate the request.
	if errs := apivalidation.ValidateUpdateActorRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}

	actorRef := resources.ActorRefFromActor(inActor)
	setSpanActorRefAttributes(ctx, actorRef)

	ctx, lease, err := acquireLease(ctx, s.impl, actorLeaseKey(actorRef), "actor")
	if err != nil {
		return nil, err
	}
	defer lease.Close()

	storedActor, err := s.impl.UpdateActor(ctx, actorRef, store.PreconditionFrom(inActor), func(toUpdate *ateapi.Actor) error {
		if toUpdate.GetMetadata().GetDeleteTime() != nil {
			return apierror.FailedPrecondition("Actor %s is being deleted", actorRef)
		}
		// Status and Metadata are server-owned fields.
		status, metadata := toUpdate.GetStatus(), toUpdate.GetMetadata()
		// Whole-object replace: clear first, so a field the client left unset is
		// cleared rather than kept from the stored actor.
		// Merge cannot smuggle in unknown fields because validation already rejected them.
		proto.Reset(toUpdate)
		proto.Merge(toUpdate, inActor)
		// Restore status and metadata from the server.
		toUpdate.Status = status
		toUpdate.Metadata = metadata
		defaults.Apply(toUpdate)
		return nil
	})
	if err != nil {
		return nil, err
	}

	setSpanActorAttributes(ctx, storedActor)

	return storedActor, err
}

func (s *ServiceImpl) UpdateActor(ctx context.Context, actorRef resources.ActorRef, precondition store.Precondition, mutate func(*ateapi.Actor) error) (*ateapi.Actor, error) {
	storedActor, err := s.store.UpdateActor(ctx, actorRef, precondition, s.validatedActorMutation(ctx, mutate))
	if err != nil {
		if mapped := actorWriteError(actorRef, err); mapped != err {
			return nil, mapped
		}
		return nil, fmt.Errorf("while updating actor: %w", err)
	}
	return storedActor, nil
}

// actorWriteError maps the store's errors for a write to the Actor actorRef to
// API errors, and returns any other error unchanged.
func actorWriteError(actorRef resources.ActorRef, err error) error {
	switch {
	case errors.Is(err, store.ErrVersionConflict), errors.Is(err, store.ErrUIDConflict):
		return apierror.Aborted("concurrent update conflict, please retry")
	case errors.Is(err, store.ErrNotFound):
		return apierror.NotFound("actor %s not found", actorRef)
	case errors.Is(err, store.ErrPreconditionRequired):
		return apierror.InvalidArgument("while updating actor %s: %v", actorRef, err)
	}
	return err
}

// validatedActorMutation wraps mutate so the Actor it writes is validated as
// an update of the stored Actor.
func (s *ServiceImpl) validatedActorMutation(ctx context.Context, mutate func(*ateapi.Actor) error) func(*ateapi.Actor) error {
	return func(toUpdate *ateapi.Actor) error {
		// Apply the mutation function to the stored value.
		oldVal := proto.CloneOf(toUpdate)
		if err := mutate(toUpdate); err != nil {
			return err
		}
		newVal := toUpdate

		// Validate the user's input before doing any further work.
		if errs := apivalidation.ValidateActorUpdate(ctx, field.NewPath("actor"), newVal, oldVal, false); len(errs) > 0 {
			return resources.ToAPIError(errs)
		}

		// Do any further work on the resource.

		// Update actor template is only allowed while the actor is suspended.
		// The repointed ref must also resolve, mirroring CreateActor's
		// check (same non-atomicity caveat; resume re-resolves and fails
		// cleanly), and the replacement's volumes and volume mounts must match
		// the old template's. It must also store snapshots under the location
		// the actor's own already live in.
		if !proto.Equal(oldVal.GetActorTemplate(), newVal.GetActorTemplate()) {
			if state := oldVal.GetStatus().GetState(); state != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
				return apierror.FailedPrecondition(
					"actor must be %s to change its actor template (got: %s)", ateapi.ActorState_ACTOR_STATE_SUSPENDED, state)
			}
			newTemplate, err := resolveActorTemplate(ctx, s.store, newVal)
			if err != nil {
				return err
			}
			if err := s.validateSnapshotLocationUnchanged(oldVal, newTemplate); err != nil {
				return err
			}
			oldTemplate, err := resolveActorTemplate(ctx, s.store, oldVal)
			switch {
			case err == nil:
				if err := validateTemplateVolumesUnchanged(oldTemplate, newTemplate); err != nil {
					return err
				}
			case errors.Is(err, errActorTemplateNotFound):
				// The old template is gone, so there is nothing left to
				// compare the volume layout against.
			default:
				return err
			}
		}

		// Validate the final value before storing it.
		if errs := apivalidation.ValidateActorUpdate(ctx, field.NewPath("actor"), newVal, oldVal, true); len(errs) > 0 {
			return resources.ToInternalError(errs)
		}

		return nil
	}
}

// validateTemplateVolumesUnchanged rejects a template repoint that changes
// the template's volumes or any container's volume mounts: an actor's
// snapshot data is laid out per the volumes and mount paths it was captured
// with, so a different layout would restore it to the wrong places. The
// volumes list must be identical, and containers present in both templates
// must keep identical mounts, order included; containers added or removed by
// the new template are unconstrained.
func validateTemplateVolumesUnchanged(oldTemplate, newTemplate *ateapi.ActorTemplate) error {
	if !slices.EqualFunc(oldTemplate.GetVolumes(), newTemplate.GetVolumes(), func(a, b *ateapi.Volume) bool {
		return proto.Equal(a, b)
	}) {
		return apierror.FailedPrecondition(
			"volumes differ between the current and the new actor template; volumes must be identical to repoint an actor")
	}

	newContainers := make(map[string]*ateapi.Container, len(newTemplate.GetContainers()))
	for _, c := range newTemplate.GetContainers() {
		newContainers[c.GetName()] = c
	}
	for _, oldC := range oldTemplate.GetContainers() {
		newC, ok := newContainers[oldC.GetName()]
		if !ok {
			continue
		}
		if !slices.EqualFunc(oldC.GetVolumeMounts(), newC.GetVolumeMounts(), func(a, b *ateapi.VolumeMount) bool {
			return proto.Equal(a, b)
		}) {
			return apierror.FailedPrecondition(
				"volume mounts of container %q differ between the current and the new actor template; volume mounts must be identical to repoint an actor", oldC.GetName())
		}
	}
	return nil
}

// validateSnapshotLocationUnchanged rejects a template repoint that would
// store the actor's next snapshots under a different location than the one it
// already owns. This is needed to not leak snapshots when the actor is deleted:
// Deleting an actor collects everything under its external snapshot prefix. If
// the location prefix ever changes, we risk leaking the snapshots under the old prefix.
func (s *ServiceImpl) validateSnapshotLocationUnchanged(actor *ateapi.Actor, newTemplate *ateapi.ActorTemplate) error {
	snap := latestSnapshot(actor.GetStatus(), func(snapshot *ateapi.Snapshot) bool {
		return snapshot.GetDurableSnapshot().GetObject().GetSnapshotUri() != ""
	})
	currentSnapshotURI := snap.GetDurableSnapshot().GetObject().GetSnapshotUri()
	if currentSnapshotURI == "" {
		return nil
	}
	currentURI, err := resources.ParseSnapshotURI(currentSnapshotURI)
	if err != nil {
		return fmt.Errorf("while parsing the external snapshot %q: %w", currentSnapshotURI, err)
	}
	// Tag-owned snapshot
	if !currentURI.OwnedBy(actorSnapshotOwner(actor)) {
		return nil
	}
	// Compared as prefixes, so a newLocation spelled with and without a
	// trailing slash counts as the same.
	newLocation, err := resolveSnapshotLocation(s.clusterSnapshotLocation, newTemplate)
	if err != nil {
		return err
	}
	// Generate what the new snapshot prefix would look like for this actor.
	nextSnapshotLocationPrefix, err := currentURI.Owner().Prefix(newLocation)
	if err != nil {
		return fmt.Errorf("while resolving the new actor template's storage location %q: %w", newLocation, err)
	}
	if nextSnapshotLocationPrefix != currentURI.OwnerPrefix() {
		return apierror.FailedPrecondition(
			"the actor's snapshots are stored under %q but the new actor template stores them under %q: the storage location must be identical to repoint an actor that owns a snapshot",
			currentURI.Location(), newLocation)
	}
	return nil
}

func (s *RPCService) DeleteActor(ctx context.Context, req *ateapi.DeleteActorRequest) (deleted *ateapi.Actor, err error) {
	if errs := apivalidation.ValidateDeleteActorRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}
	start := time.Now()
	// Template dims only once the record resolved: the request names only the
	// actor, so failures before the load carry none. No pool pair, even when
	// the deleted actor still held a worker.
	defer func() {
		var attrs []attribute.KeyValue
		if deleted != nil {
			attrs = append(attrs,
				ateattr.TemplateNameKey.String(deleted.GetActorTemplate().GetName()),
				ateattr.TemplateAtespaceKey.String(deleted.GetActorTemplate().GetAtespace()),
			)
		}
		s.instruments.recordLifecycleOp(ctx, ateattr.OperationDelete, start, err, attrs...)
	}()
	actorRef := resources.ActorRefFromObjectRef(req.GetActor())
	setSpanActorRefAttributes(ctx, actorRef)

	deleted, err = s.actorWorkflow.DeleteActor(ctx, actorRef, toDeletePreconditions(req.GetOptions()))
	if err != nil {
		return nil, err
	}

	return deleted, nil
}

func (s *ServiceImpl) MarkActorForDeletion(ctx context.Context, actorRef resources.ActorRef, precondition store.DeletePreconditions, mutate func(*ateapi.Actor) error) (*ateapi.Actor, error) {
	return s.store.MarkActorForDeletion(ctx, actorRef, precondition, mutate)
}

func (s *ServiceImpl) DeleteActor(ctx context.Context, actorRef resources.ActorRef, precondition store.DeletePreconditions) (*ateapi.Actor, error) {
	return s.store.DeleteActor(ctx, actorRef, precondition)
}

func (s *RPCService) ResumeActor(ctx context.Context, req *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error) {
	if errs := apivalidation.ValidateResumeActorRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}
	actorRef := resources.ActorRefFromObjectRef(req.GetActor())
	setSpanActorRefAttributes(ctx, actorRef)

	actor, resumed, err := s.actorWorkflow.ResumeActor(ctx, actorRef)
	if err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			return nil, apierror.Aborted("concurrent update conflict, please retry")
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil, apierror.NotFound("Actor %s not found", actorRef)
		}
		return nil, err
	}

	setSpanActorAttributes(ctx, actor)
	return &ateapi.ResumeActorResponse{Actor: actor, Resumed: resumed}, nil
}

func (s *RPCService) SuspendActor(ctx context.Context, req *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error) {
	if errs := apivalidation.ValidateSuspendActorRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}
	actorRef := resources.ActorRefFromObjectRef(req.GetActor())
	setSpanActorRefAttributes(ctx, actorRef)

	actor, err := s.actorWorkflow.SuspendActor(ctx, actorRef, suspendModeFromOptions(req.GetOptions()))
	if err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			return nil, apierror.Aborted("concurrent update conflict, please retry")
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil, apierror.NotFound("Actor %s not found", actorRef)
		}
		return nil, err
	}
	setSpanActorAttributes(ctx, actor)
	return &ateapi.SuspendActorResponse{Actor: actor}, nil
}

// CrashActorReportedByWorker crashes a RUNNING actor whose sandbox died, on
// behalf of the worker hosting it. The caller authorizes the worker.
func (s *RPCService) CrashActorReportedByWorker(ctx context.Context, actorRef resources.ActorRef, actorUID, workerName, message string) (*ateapi.Actor, error) {
	setSpanActorRefAttributes(ctx, actorRef)
	actor, err := s.actorWorkflow.CrashActorReportedByWorker(ctx, actorRef, actorUID, workerName, message)
	if err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			return nil, apierror.Aborted("concurrent update conflict, please retry")
		}
		return nil, err
	}
	setSpanActorAttributes(ctx, actor)
	return actor, nil
}

func (s *RPCService) RevertActor(ctx context.Context, req *ateapi.RevertActorRequest) (*ateapi.RevertActorResponse, error) {
	if errs := apivalidation.ValidateRevertActorRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}
	actorRef := resources.ActorRefFromObjectRef(req.GetActor())
	setSpanActorRefAttributes(ctx, actorRef)

	actor, err := s.actorWorkflow.RevertActor(ctx, actorRef)
	if err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			return nil, apierror.Aborted("concurrent update conflict, please retry")
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil, apierror.NotFound("Actor %s not found", actorRef)
		}
		return nil, err
	}
	setSpanActorAttributes(ctx, actor)
	return &ateapi.RevertActorResponse{Actor: actor}, nil
}

func (s *RPCService) MintActorJwt(ctx context.Context, req *ateapi.MintActorJwtRequest) (*ateapi.MintActorJwtResponse, error) {
	if errs := apivalidation.ValidateMintActorJWTRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}

	// With authz enforced, the interceptor requires can_mint_actor_jwt on the
	// actor, which only the egress gateway holds.

	// Verify that this actor exists in the store.  It doesn't need to be
	// running, since we may need to issue JWTs during actor boot / resume.
	dbActor, err := s.impl.GetActor(ctx, resources.ActorRefFromObjectRef(req.GetActor()))
	if errors.Is(err, store.ErrNotFound) {
		return nil, apierror.NotFound("actor not found")
	} else if err != nil {
		return nil, fmt.Errorf("while retrieving actor: %w", err)
	}

	// We only issue tokens with audience bindings.
	if len(req.GetAudiences()) == 0 {
		return nil, fmt.Errorf("at least one audience must be requested")
	}

	// JWT timestamps have one-second resolution; truncating keeps expire_time
	// equal to the exp claim.
	now := time.Now().Truncate(time.Second)
	expiresAt := now.Add(time.Duration(req.GetExpirationSeconds()) * time.Second)
	actorClaims := &actoridjwt.Claims{
		Kind:       actoridjwt.KindActor,
		Issuer:     s.actorJWTIssuer,
		Subject:    fmt.Sprintf("%s/%s/%s", actoridjwt.KindActor.SubjectPrefix(), dbActor.GetMetadata().GetAtespace(), dbActor.GetMetadata().GetName()),
		Audiences:  req.GetAudiences(),
		Expiration: expiresAt,
		NotBefore:  now.Add(-5 * time.Minute),
		IssuedAt:   now,
		JTI:        rand.Text(),

		Substrate: actoridjwt.SubstrateClaims{
			Atespace:  dbActor.GetMetadata().GetAtespace(),
			ActorName: dbActor.GetMetadata().GetName(),
			ActorUID:  dbActor.GetMetadata().GetUid(),
		},
	}

	actorJWT, err := s.actorIDJWTPool.SignJWT(actorClaims)
	if err != nil {
		return nil, fmt.Errorf("while signing actor JWT: %w", err)
	}

	return &ateapi.MintActorJwtResponse{
		ActorJwt:   actorJWT,
		ExpireTime: timestamppb.New(expiresAt),
	}, nil
}
