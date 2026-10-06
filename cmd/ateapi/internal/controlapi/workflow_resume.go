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

package controlapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/scheduling"
	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/ateattr"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
)

// resumeSnapshotSource is the boot source resolved once by loadActorForResume
// and passed by value to the restore step — never mutated after resolution.
type resumeSnapshotSource struct {
	// SnapshotURI is the storage location of the durable snapshot to restore
	// from: the actor's latest snapshot, including a tag borrowed at creation.
	// Zero means cold boot from the spec (unless the actor holds a local
	// snapshot, which takes precedence at restore).
	SnapshotURI resources.SnapshotURI
	Scope       ateapipb.SnapshotContentScope
	// TemplateReplaced is true when the external snapshot's recorded template
	// UID differs from the actor's current template.
	TemplateReplaced bool
}

// restoreTelemetry labels the restore operation for the resume lifecycle
// metric. WireSnapshotScope describes the restore requested, not the stored
// snapshot's scope: a full snapshot restored under a replaced template goes
// out as data.
type restoreTelemetry struct {
	SnapshotKind      string
	WireSnapshotScope string
}

// resumeTiming is the wall clock of one ResumeActor call, step by step. It is
// logged as "Resume timing breakdown" so resume latency can be attributed from
// logs: the step spans carry the same split per trace but do not aggregate.
type resumeTiming struct {
	getActor, leaseAcquire, load, volumesCreate, assign, volumesAttach, finalize, leaseRelease time.Duration
	// The successful assignment attempt's calls; a retried attempt's are dropped.
	schedule, bind, assignUpdate time.Duration
	// assignAttempts counts assignWorkerAttempt calls; zero when the actor was
	// already RESUMING with a valid worker.
	assignAttempts            int
	ateletDial, ateletRestore time.Duration
	// nodePreference is the successful attempt's same-node preference outcome
	// (see nodePreferenceOutcome); empty when no worker was scheduled.
	nodePreference string
}

const (
	resumeDurationKeyPrefix = "ate.actor.resume.duration."
	resumeAssignAttemptsKey = "ate.actor.resume.assign_attempts"
	// nodePreferenceKey labels whether a resume landed on the node its external
	// snapshot was uploaded from (hit), elsewhere (miss), or had no preference
	// to honor (none).
	nodePreferenceKey = "ate.scheduler.node_preference"

	nodePreferenceHit  = "hit"
	nodePreferenceMiss = "miss"
	nodePreferenceNone = "none"
)

// nodePreferenceOutcome classifies the worker Schedule picked against the
// constraints' preferred nodes.
func nodePreferenceOutcome(worker *ateapipb.Worker, constraints scheduling.Constraints) string {
	switch {
	case len(constraints.PreferredNodes) == 0:
		return nodePreferenceNone
	case slices.Contains(constraints.PreferredNodes, worker.GetNodeName()):
		return nodePreferenceHit
	default:
		return nodePreferenceMiss
	}
}

// logAttrs renders the record: the actor identity, one float-seconds attr per
// step that ran, the attempt count, and on failure the gRPC code as error.type.
// actor may be nil when the first read failed.
func (tm *resumeTiming) logAttrs(actorRef resources.ActorRef, actor *ateapipb.Actor, total time.Duration, err error) []slog.Attr {
	attrs := ateattr.ActorRefLogAttrs(actorRef)
	if actor != nil {
		attrs = ateattr.ActorLogAttrs(resources.ActorAttributionFromActor(actor))
	}
	phases := []struct {
		name string
		d    time.Duration
	}{
		{"get_actor", tm.getActor},
		{"lease_acquire", tm.leaseAcquire},
		{"load", tm.load},
		{"volumes_create", tm.volumesCreate},
		{"assign", tm.assign},
		{"schedule", tm.schedule},
		{"bind", tm.bind},
		{"assign_update", tm.assignUpdate},
		{"volumes_attach", tm.volumesAttach},
		{"atelet_dial", tm.ateletDial},
		{"atelet_restore", tm.ateletRestore},
		{"finalize", tm.finalize},
		{"lease_release", tm.leaseRelease},
		{"total", total},
	}
	for _, p := range phases {
		if p.d == 0 {
			continue
		}
		attrs = append(attrs, slog.Float64(resumeDurationKeyPrefix+p.name, p.d.Seconds()))
	}
	attrs = append(attrs, slog.Int(resumeAssignAttemptsKey, tm.assignAttempts))
	if tm.nodePreference != "" {
		attrs = append(attrs, slog.String(nodePreferenceKey, tm.nodePreference))
	}
	if err != nil {
		attrs = append(attrs, slog.String(string(ateattr.ErrorTypeKey), status.Code(err).String()))
	}
	return attrs
}

// ResumeActor executes the workflow to resume a suspended actor. Idempotent:
// a re-entered workflow fast-forwards past the steps a previous attempt
// completed, deriving progress from the persisted actor alone.
// WakeSpanName is the span ateapi opens around a resume that has to restore
// the actor. With TraceEveryWake it roots a trace of its own (see startWake).
const WakeSpanName = "ActorWake"

// TraceEveryWake makes startWake root a new trace per wake instead of
// nesting the wake under the request's trace. ateapi's --trace-every-wake
// flag sets it; the sampler installed with it always samples WakeSpanName.
var TraceEveryWake = true

const wakeTraceIDKey = "ate.wake.trace_id"

// startWake opens the wake span for a resume that is past the fast path. Under
// TraceEveryWake it is a new root linked to the request's span, and the
// request's span is linked back and tagged with the wake's trace id, so a
// sampled request leads to its wake and a wake leads to the request that
// caused it even when that request was not sampled. The snapshot the wake
// restores from links to the trace that produced it, when that was recorded.
func startWake(ctx context.Context, actor *ateapipb.Actor) (context.Context, trace.Span) {
	reqSpan := trace.SpanFromContext(ctx)
	attrs := ateattr.ActorAttributes(actor)
	attrs = append(attrs, attribute.String("ate.actor.state", actor.GetStatus().GetState().String()))
	opts := []trace.SpanStartOption{trace.WithAttributes(attrs...)}
	if snap := actor.GetStatus().GetExternalSnapshot(); actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_SUSPENDED && snap.GetProducedByTraceId() != "" {
		if link, ok := snapshotProducerLink(snap); ok {
			opts = append(opts, trace.WithLinks(link))
		}
	}
	// A new root only for a state a resume can actually wake from. A CRASHED
	// or DELETING actor fails at the next step, and the router retries such a
	// resume several times a second while the request is parked; rooting a
	// trace per attempt would bury the real wakes under error traces.
	if !TraceEveryWake || !wakeableState(actor.GetStatus().GetState()) {
		return otel.Tracer("controlapi").Start(ctx, WakeSpanName, opts...)
	}
	opts = append(opts, trace.WithNewRoot(), trace.WithLinks(trace.Link{
		SpanContext: reqSpan.SpanContext(),
		Attributes:  []attribute.KeyValue{attribute.String("ate.link.kind", "request")},
	}))
	wakeCtx, wakeSpan := otel.Tracer("controlapi").Start(ctx, WakeSpanName, opts...)
	reqSpan.AddLink(trace.Link{
		SpanContext: wakeSpan.SpanContext(),
		Attributes:  []attribute.KeyValue{attribute.String("ate.link.kind", "wake")},
	})
	reqSpan.SetAttributes(attribute.String(wakeTraceIDKey, wakeSpan.SpanContext().TraceID().String()))
	return wakeCtx, wakeSpan
}

// wakeableState reports whether a resume from state restores the actor.
func wakeableState(state ateapipb.ActorState) bool {
	switch state {
	case ateapipb.ActorState_ACTOR_STATE_PAUSED, ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
		return true
	}
	return false
}

// snapshotProducerLink builds the link from a wake to the trace that produced
// the snapshot it restores. Malformed ids yield no link rather than an error:
// the ids are a convenience for a reader, never a condition of the resume.
func snapshotProducerLink(snap *ateapipb.ExternalSnapshot) (trace.Link, bool) {
	tid, err := trace.TraceIDFromHex(snap.GetProducedByTraceId())
	if err != nil {
		return trace.Link{}, false
	}
	sid, err := trace.SpanIDFromHex(snap.GetProducedBySpanId())
	if err != nil {
		return trace.Link{}, false
	}
	return trace.Link{
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true}),
		Attributes:  []attribute.KeyValue{attribute.String("ate.link.kind", "snapshot_producer")},
	}, true
}

// producerIDs returns the trace and span id of the span in ctx for recording
// on a snapshot it produced, or empty strings when ctx carries no valid span.
func producerIDs(ctx context.Context) (traceID, spanID string) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}

func (w *ActorWorkflow) ResumeActor(ctx context.Context, actorRef resources.ActorRef) (_ *ateapipb.Actor, resumed bool, err error) {
	start := time.Now()
	var actor *ateapipb.Actor
	var actorTemplate *ateapipb.ActorTemplate
	var tele restoreTelemetry
	var wasRunning bool
	var tm resumeTiming
	var wakeTraceID string

	// Recorded before the lease so lease contention still counts as an attempt.
	// Clean already-running no-ops are skipped: the router resumes per routed
	// request, and recording those would sample at router QPS and bury
	// cold-resume latency. The timing record follows the same rule.
	defer func() {
		if err == nil && wasRunning {
			return
		}
		w.instruments.recordLifecycleOp(ctx, ateattr.OperationResume, start, err,
			lifecycleOpAttrs(actor, actorTemplate, tele.SnapshotKind, tele.WireSnapshotScope)...)
		attrs := tm.logAttrs(actorRef, actor, time.Since(start), err)
		if wakeTraceID != "" {
			attrs = append(attrs, slog.String(wakeTraceIDKey, wakeTraceID))
		}
		slog.LogAttrs(ctx, slog.LevelInfo, "Resume timing breakdown", attrs...)
	}()

	// Routed requests call ResumeActor even when the actor is already running.
	// Read before taking the distributed lease so that hot-path checks do not
	// upsert and delete a PostgreSQL lease row. Any state that needs work is read
	// again under the lease below.
	t := time.Now()
	actor, err = w.store.GetActor(ctx, actorRef)
	tm.getActor = time.Since(t)
	if err != nil {
		return nil, false, err
	}
	if wasRunning = actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_RUNNING; wasRunning {
		return actor, false, nil
	}

	t = time.Now()
	leaseCtx, lease, err := w.acquireActorLease(ctx, actorRef)
	tm.leaseAcquire = time.Since(t)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		t := time.Now()
		lease.Close()
		tm.leaseRelease = time.Since(t)
	}()

	var src resumeSnapshotSource
	t = time.Now()
	actor, actorTemplate, src, err = w.loadActorForResume(leaseCtx, actorRef)
	tm.load = time.Since(t)
	if err != nil {
		return nil, false, err
	}
	if wasRunning = actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_RUNNING; wasRunning {
		return actor, false, nil
	}
	// Past the fast path: this resume restores the actor. Everything from here
	// runs under the wake span (its own trace under TraceEveryWake).
	var wakeSpan trace.Span
	leaseCtx, wakeSpan = startWake(leaseCtx, actor)
	wakeTraceID = wakeSpan.SpanContext().TraceID().String()
	defer func() {
		if err != nil {
			wakeSpan.RecordError(err)
			wakeSpan.SetStatus(otelcodes.Error, err.Error())
		}
		wakeSpan.End()
	}()
	var created *ateapipb.Actor
	t = time.Now()
	created, err = w.ensureVolumesCreated(leaseCtx, actorRef, actor, actorTemplate)
	tm.volumesCreate = time.Since(t)
	if err != nil {
		return nil, false, err
	}
	actor = created
	var worker *ateapipb.Worker
	var assigned *ateapipb.Actor
	t = time.Now()
	assigned, worker, err = w.ensureWorkerAssigned(leaseCtx, actorRef, actor, actorTemplate, &tm)
	tm.assign = time.Since(t)
	if err != nil {
		return nil, false, err
	}
	actor = assigned
	t = time.Now()
	err = w.ensureVolumesAttached(leaseCtx, actor, worker, actorTemplate)
	tm.volumesAttach = time.Since(t)
	if err != nil {
		return nil, false, err
	}
	if tele, err = w.ensureAteletRestored(leaseCtx, actorRef, actor, actorTemplate, src, &tm); err != nil {
		return nil, false, err
	}
	var running *ateapipb.Actor
	t = time.Now()
	running, err = w.finalizeRunning(leaseCtx, actorRef)
	tm.finalize = time.Since(t)
	if err != nil {
		return nil, false, err
	}
	actor = running
	return actor, true, nil
}

// validateGoldenSnapshotScope rejects a golden snapshot that does not carry
// the guest state (memory + fs delta) a restore needs. Golden actors always
// commit Full (commitSnapshotScope), so this only trips on golden snapshots
// taken before that rule existed — surface a clear error instead of shipping
// a restore request atelet would reject (or that would boot an empty guest).
func validateGoldenSnapshotScope(snapshot *ateapipb.ExternalSnapshot) error {
	scope := snapshot.GetContentScope()
	switch scope {
	case ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_UNSPECIFIED,
		ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL:
		return nil
	default:
		return status.Errorf(codes.FailedPrecondition,
			"ActorTemplate golden snapshot %q was taken with scope %s, not Full; regenerate the golden snapshot",
			snapshot.GetSnapshotUri(), scope)
	}
}

// loadActorForResume fetches the current actor record and its template, and
// resolves the boot source for the pending restore.
func (w *ActorWorkflow) loadActorForResume(ctx context.Context, actorRef resources.ActorRef) (_ *ateapipb.Actor, _ *ateapipb.ActorTemplate, _ resumeSnapshotSource, err error) {
	ctx, done := stepSpan(ctx, "LoadActorForResume")
	defer func() { err = done(err) }()

	var src resumeSnapshotSource
	actor, err := w.store.GetActor(ctx, actorRef)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, src, status.Errorf(codes.NotFound, "Actor %s not found", actorRef)
		}
		return nil, nil, src, fmt.Errorf("while getting actor from DB: %w", err)
	}

	// If the actor is already running, there is no pending restore to prepare
	// for. Short-circuit immediately to avoid unnecessary store reads for snapshots
	// and template resolution on the hot resume path.
	if actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_RUNNING {
		return actor, nil, src, nil
	}

	actorTemplate, err := resolveActorTemplate(ctx, w.store, actor)
	if err != nil {
		return nil, nil, src, err
	}
	if uri := actor.GetStatus().GetExternalSnapshot().GetSnapshotUri(); uri != "" {
		if src.SnapshotURI, err = resources.ParseSnapshotURI(uri); err != nil {
			return nil, nil, src, status.Errorf(codes.DataLoss, "Actor %s external snapshot: %v", actorRef, err)
		}
		src.Scope = actor.GetStatus().GetExternalSnapshot().GetContentScope()
		capturedUnder := actor.GetStatus().GetExternalSnapshot().GetActorTemplateUid()
		src.TemplateReplaced = capturedUnder != "" && capturedUnder != actorTemplate.GetMetadata().GetUid()
	}

	return actor, actorTemplate, src, nil
}

// ensureVolumesCreated provisions any initial actor volumes that are in
// PENDING state, persisting the resulting volume state (even when creation
// partially failed, so progress is not lost) and returning the stored copy.
func (w *ActorWorkflow) ensureVolumesCreated(ctx context.Context, actorRef resources.ActorRef, actor *ateapipb.Actor, actorTemplate *ateapipb.ActorTemplate) (_ *ateapipb.Actor, err error) {
	ctx, done := stepSpan(ctx, "CreateVolumes")
	defer func() { err = done(err) }()

	pending := false
	for _, vol := range actor.GetStatus().GetActorVolumes() {
		if vol.GetStatus() == ateapipb.ExternalVolume_STATUS_PENDING {
			pending = true
			break
		}
	}
	if !pending {
		markSkipped(ctx, "no volumes awaiting creation")
		return actor, nil
	}

	volumes, createErr := createActorVolumes(ctx, w.pluginRegistry, w.storageClassLister, actor.GetMetadata().GetUid(), actorTemplate, actor.GetStatus().GetActorVolumes())
	// createActorVolumes reports the state it got to even when it fails, so both
	// paths persist the same field.
	updatePrecondition := store.PreconditionFrom(actor)
	persistVolumes := func(toUpdate *ateapipb.Actor) error {
		toUpdate.Status.ActorVolumes = volumes
		return nil
	}
	if createErr != nil {
		// Even if volume creation failed, we still want to persist any updated volume state.
		if _, updateErr := w.store.UpdateActor(ctx, actorRef, updatePrecondition, persistVolumes); updateErr != nil {
			slog.ErrorContext(ctx, "failed to update actor volumes on volume creation failure in resume", slog.Any("error", updateErr))
		}
		return nil, createErr
	}
	storedActor, updateErr := w.store.UpdateActor(ctx, actorRef, updatePrecondition, persistVolumes)
	if updateErr != nil {
		if errors.Is(updateErr, store.ErrVersionConflict) {
			return nil, status.Error(codes.Aborted, "concurrent update conflict, please retry")
		}
		return nil, fmt.Errorf("while updating actor after volume creation: %w", updateErr)
	}
	return storedActor, nil
}

// ensureWorkerAssigned leaves the actor RESUMING with a validated, live,
// owned, and eligible worker.
//
// A RESUMING actor was assigned by a previous attempt: its persisted
// assignment is revalidated (worker still exists, not draining, still owned
// by this actor UID, still eligible for the actor's constraints) and reused;
// a stale assignment crashes the actor. A SUSPENDED or PAUSED actor goes
// through scheduling, claiming the worker and persisting RESUMING with the
// assignment. A version conflict there is retried under a bounded backoff
// only after re-reading the actor and revalidating it can still be resumed —
// the conflicting writer may have crashed, drained, or deleted it.
func (w *ActorWorkflow) ensureWorkerAssigned(ctx context.Context, actorRef resources.ActorRef, actor *ateapipb.Actor, actorTemplate *ateapipb.ActorTemplate, tm *resumeTiming) (_ *ateapipb.Actor, _ *ateapipb.Worker, err error) {
	ctx, done := stepSpan(ctx, "AssignWorker")
	defer func() { err = done(err) }()

	switch actor.GetStatus().GetState() {
	case ateapipb.ActorState_ACTOR_STATE_RESUMING:
		worker, err := w.validateAssignedWorker(ctx, actorRef, actor, actorTemplate)
		if err != nil {
			return nil, nil, err
		}
		markSkipped(ctx, "actor already RESUMING with a valid worker assignment")
		return actor, worker, nil
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ateapipb.ActorState_ACTOR_STATE_PAUSED:
	default:
		return nil, nil, status.Errorf(codes.FailedPrecondition, "AssignWorker prerequisite not met for Actor: %s (got: %v, want %s or %s)", actorRef, actor.GetStatus().GetState(), ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ateapipb.ActorState_ACTOR_STATE_PAUSED)
	}

	// Bound contention retries to about three seconds.
	backoff := wait.Backoff{
		Steps:    12,
		Duration: 15 * time.Millisecond,
		Factor:   2.0,
		Jitter:   1.0,
		Cap:      250 * time.Millisecond,
	}
	var assignedActor *ateapipb.Actor
	var assignedWorker *ateapipb.Worker
	err = wait.ExponentialBackoff(backoff, func() (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		tm.assignAttempts++
		attemptActor, attemptWorker, attemptErr := w.assignWorkerAttempt(ctx, actorRef, actor, actorTemplate, tm)
		if attemptErr == nil {
			assignedActor, assignedWorker = attemptActor, attemptWorker
			return true, nil
		}
		if errors.Is(attemptErr, store.ErrVersionConflict) || errors.Is(attemptErr, errWorkerFilledUp) {
			if attemptActor != nil {
				actor = attemptActor // retry with the refreshed actor
			}
			return false, nil
		}
		return false, attemptErr
	})
	if err != nil {
		if wait.Interrupted(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, store.ErrVersionConflict
		}
		return nil, nil, err
	}
	return assignedActor, assignedWorker, nil
}

// validateAssignedWorker checks a RESUMING actor's persisted assignment
// against the current worker record. Every invalid outcome crashes the actor:
// a RESUMING actor whose worker vanished, drained, was reassigned, or is no
// longer eligible can never make progress on its own.
func (w *ActorWorkflow) validateAssignedWorker(ctx context.Context, actorRef resources.ActorRef, actor *ateapipb.Actor, actorTemplate *ateapipb.ActorTemplate) (*ateapipb.Worker, error) {
	assignment := actor.GetStatus().GetWorkerAssignment()
	if assignment == nil {
		slog.ErrorContext(ctx, "expected a worker assignment on a RESUMING actor, found none")

		// Crash the actor if its worker assignment is missing. We should never be in this state.
		if cerr := crashActor(ctx, w.store, actorRef, ateattr.OperationResume, crashMessageWorkerAssignmentMissing); cerr != nil {
			return nil, cerr
		}
		return nil, status.Errorf(codes.Aborted, "actor %s crashed", actorRef)
	}

	worker, err := w.store.GetWorker(ctx, assignment.GetWorker().GetName())
	if err != nil {
		// Crash the actor if it was assigned to a deleted pod.
		if errors.Is(err, store.ErrNotFound) {
			if cerr := crashActor(ctx, w.store, actorRef, ateattr.OperationResume, crashMessageWorkerGone); cerr != nil {
				return nil, cerr
			}
			return nil, status.Errorf(codes.Aborted, "actor %s crashed", actorRef)
		}
		return nil, fmt.Errorf("failed to get already assigned worker for actor %w", err)
	}
	if worker.GetStatus().GetState() == ateapipb.WorkerState_WORKER_STATE_DRAINING {
		slog.InfoContext(ctx, "Assigned worker is draining; crashing actor",
			slog.String("actor", actorRef.String()),
			slog.String("worker", worker.GetWorkerNamespace()+"/"+worker.GetWorkerPod()))
		if cerr := crashActor(ctx, w.store, actorRef, ateattr.OperationResume, crashMessageWorkerDraining); cerr != nil {
			return nil, cerr
		}
		return nil, status.Errorf(codes.Aborted, "actor %s crashed", actorRef.String())
	}
	// Verify the worker is still hosting this Actor.
	hosted, err := workerHostsActor(ctx, w.store, worker.GetMetadata().GetName(), actor.GetMetadata().GetUid())
	if err != nil {
		return nil, err
	}
	if !hosted {
		slog.ErrorContext(ctx, "crashing actor because its assigned worker no longer hosts it",
			slog.String("worker", worker.GetWorkerPod()))
		if cerr := crashActor(ctx, w.store, actorRef, ateattr.OperationResume, crashMessageWorkerReassigned); cerr != nil {
			return nil, fmt.Errorf("while crashing actor: %w", cerr)
		}
		return nil, status.Errorf(codes.Aborted, "actor %s crashed", actorRef)
	}
	constraints, err := schedulingConstraints(actor, actorTemplate)
	if err != nil {
		return nil, err
	}
	if !w.scheduler.Applies(worker, constraints) {
		slog.ErrorContext(ctx, "crashing actor because previously assigned worker is not eligible anymore")
		// If that worker's pool is no longer eligible (e.g. the actor's
		// worker_selector was updated after the failed attempt), release it back
		// to the free pool instead of leaving it claimed forever — nothing else
		// reclaims a healthy worker whose actor moved on to a different pool.
		if _, err := w.store.ReleaseActorFromWorker(ctx, worker.GetMetadata().GetName(), actor.GetMetadata().GetUid()); err != nil {
			return nil, fmt.Errorf("while releasing stale worker assignment: %w", err)
		}
		if cerr := crashActor(ctx, w.store, actorRef, ateattr.OperationResume, crashMessageWorkerIneligible); cerr != nil {
			return nil, fmt.Errorf("while crashing actor: %w", cerr)
		}
		return nil, status.Errorf(codes.Aborted, "actor %s crashed", actorRef)
	}
	return worker, nil
}

// admittedResources is what an assignment books against its worker, or nil
// when the actor declared no limits and so reserves nothing.
func admittedResources(constraints scheduling.Constraints) *ateapipb.Resources {
	return constraints.Limits
}

// workerHoldingStaleClaim recovers a claim written before the Actor update.
// It releases the claim if the Worker is no longer eligible.
func (w *ActorWorkflow) workerHoldingStaleClaim(ctx context.Context, actor *ateapipb.Actor, constraints scheduling.Constraints) (*ateapipb.Worker, error) {
	actorUID := actor.GetMetadata().GetUid()
	workerName, err := w.store.FindWorkerHostingActor(ctx, actorUID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("while looking for a worker already hosting actor %q: %w", actorUID, err)
	}
	worker, err := w.store.GetWorker(ctx, workerName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil // the worker went away with its claim
		}
		return nil, fmt.Errorf("while reading worker %q holding a stale claim: %w", workerName, err)
	}

	// The Actor's allocation makes HasRoom unsuitable for an existing claim.
	if w.scheduler.Applies(worker, constraints) {
		return worker, nil
	}

	_, err = w.store.ReleaseActorFromWorker(ctx, workerName, actorUID)
	if err != nil {
		return nil, fmt.Errorf("while releasing stale claim on worker %q: %w", workerName, err)
	}
	return nil, nil
}

// schedulerRecordable excludes retried version conflicts: the assignment loop
// re-runs attempts transparently on store.ErrVersionConflict, so counting
// those attempts would inflate the error rate and double-count the eventual
// success.
func schedulerRecordable(err error) bool {
	return !errors.Is(err, store.ErrVersionConflict)
}

// assignWorkerAttempt makes one attempt at claiming a worker for the actor
// and persisting RESUMING with the assignment. On a version conflict it
// re-reads the actor: if the fresh copy can still be resumed the refreshed
// actor is returned along with the conflict so the caller retries with clean
// inputs; any other status aborts the resume. tm receives the attempt's call
// durations only when it succeeds.
func (w *ActorWorkflow) assignWorkerAttempt(ctx context.Context, actorRef resources.ActorRef, actor *ateapipb.Actor, actorTemplate *ateapipb.ActorTemplate, tm *resumeTiming) (_ *ateapipb.Actor, _ *ateapipb.Worker, err error) {
	start := time.Now()
	var dSchedule, dBind, dUpdate time.Duration
	var nodePreference string
	outcome := ateattr.SchedulerOutcomeError
	poolNamespace := ""
	pool := ""
	class := ""
	if actorTemplate != nil {
		class = sandboxClassString(actorTemplate.GetSandboxConfig().GetSandboxClass())
	}
	defer func() {
		if schedulerRecordable(err) {
			w.instruments.recordSchedulerAssignment(ctx, start, outcome, poolNamespace, pool, class, err)
		}
	}()

	constraints, err := schedulingConstraints(actor, actorTemplate)
	if err != nil {
		return nil, nil, err
	}

	assignedWorker, err := w.workerHoldingStaleClaim(ctx, actor, constraints)
	if err != nil {
		return nil, nil, err
	}
	if assignedWorker == nil {
		t := time.Now()
		pickedWorker, err := w.scheduler.Schedule(ctx, constraints)
		dSchedule = time.Since(t)
		if err != nil {
			if errors.Is(err, scheduling.ErrNoCapacity) {
				outcome = ateattr.SchedulerOutcomeNoFreeWorker
				return nil, nil, status.Errorf(codes.ResourceExhausted, "no free workers available")
			}
			return nil, nil, err
		}

		assignedWorker = pickedWorker
		nodePreference = nodePreferenceOutcome(pickedWorker, constraints)
		slog.InfoContext(ctx, "Picked worker",
			slog.Any("worker", pickedWorker.String()),
			slog.String(nodePreferenceKey, nodePreference))
	}

	assignment := &ateapipb.ActorAssignment{
		Actor: &ateapipb.ObjectRef{
			Atespace: actor.GetMetadata().GetAtespace(),
			Name:     actor.GetMetadata().GetName(),
		},
		ActorUid: actor.GetMetadata().GetUid(),
		// Record what this claim reserves so release returns the same amount.
		Resources: admittedResources(constraints),
	}
	assignment.ActorTemplateRef = actorTemplateObjectRef(actor)

	// The candidate came from a watch-fed cache, so it may already be full or no
	// longer eligible. The store re-asks under the Worker's row lock, where the
	// answer holds until the bind commits, so nothing here needs a fresh read
	// and two claims for the last place cannot both be admitted.
	admit := func(fresh *ateapipb.Worker) error {
		if !w.scheduler.Applies(fresh, constraints) || !w.scheduler.HasRoom(fresh, constraints) {
			return errWorkerFilledUp
		}
		return nil
	}
	t := time.Now()
	err = w.store.BindActorToWorker(ctx, assignedWorker.GetMetadata().GetName(), assignment, admit)
	dBind = time.Since(t)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.workerCache.Forget(assignedWorker.GetMetadata().GetName())
			return nil, nil, fmt.Errorf("selected worker disappeared before claim: %w", store.ErrVersionConflict)
		}
		return nil, nil, err
	}

	newAssignment := workerAssignmentFrom(assignedWorker)
	// The cached Worker may predate a raised epoch; the bind read it under the
	// Worker's row lock.
	newAssignment.WorkerEpoch = assignment.GetWorkerEpoch()
	t = time.Now()
	storedActor, err := w.store.UpdateActor(ctx, actorRef, store.PreconditionFrom(actor), func(toUpdate *ateapipb.Actor) error {
		toUpdate.Status.State = ateapipb.ActorState_ACTOR_STATE_RESUMING
		toUpdate.Status.WorkerAssignment = newAssignment
		return nil
	})
	dUpdate = time.Since(t)
	if err != nil {
		if !errors.Is(err, store.ErrVersionConflict) {
			return nil, nil, err
		}
		// refresh the version of actor to avoid always failure in rest retries.
		fresh, gerr := w.store.GetActor(ctx, actorRef)
		if gerr != nil {
			slog.WarnContext(ctx, "Failed to refresh actor after assignment conflict", slog.Any("err", gerr))
			return nil, nil, err
		}
		switch fresh.GetStatus().GetState() {
		case ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ateapipb.ActorState_ACTOR_STATE_PAUSED:
			slog.InfoContext(ctx, "Retrying assignment due to actor version conflict", slog.Any("actor", actorRef))
			return fresh, nil, err
		default:
			return nil, nil, status.Errorf(codes.Aborted, "actor %s is %s and can no longer be resumed", actorRef, fresh.GetStatus().GetState())
		}
	}
	poolNamespace = assignedWorker.GetWorkerNamespace()
	pool = assignedWorker.GetWorkerPool()
	outcome = ateattr.SchedulerOutcomeAssigned
	tm.schedule, tm.bind, tm.assignUpdate = dSchedule, dBind, dUpdate
	tm.nodePreference = nodePreference
	logActorStateChanged(ctx, storedActor, ateattr.OperationResume)
	return storedActor, assignedWorker, nil
}

// errWorkerFilledUp reports that the Worker the scheduler picked would not take
// the Actor once the store asked under its row lock. Retryable: the next
// attempt re-runs scheduling.
var errWorkerFilledUp = errors.New("picked worker no longer has room")

func workerAssignmentFrom(w *ateapipb.Worker) *ateapipb.WorkerAssignment {
	return &ateapipb.WorkerAssignment{
		Worker:          &ateapipb.ObjectRef{Name: w.GetMetadata().GetName()},
		WorkerNamespace: w.GetWorkerNamespace(),
		WorkerPool:      w.GetWorkerPool(),
		WorkerPod:       w.GetWorkerPod(),
		WorkerPodUid:    w.GetWorkerPodUid(),
		WorkerPodIps:    slices.Clone(w.GetIps()),
		NodeName:        w.GetNodeName(),
	}
}

// actorResourceLimits returns the actor's declared CPU (millicores) and memory
// (bytes) limits from its ActorTemplate, or 0 for a dimension the template did
// not set. These size the sandbox, which takes the two scalars the runtimes
// understand rather than the named set placement accounts in.
func actorResourceLimits(tmpl *ateapipb.ActorTemplate) (cpuMilli, memBytes int64, err error) {
	for _, limit := range tmpl.GetResources().GetLimits() {
		q, perr := resource.ParseQuantity(limit.GetQuantity())
		if perr != nil {
			return 0, 0, fmt.Errorf("invalid template resource limit %s=%q: %w", limit.GetName(), limit.GetQuantity(), perr)
		}
		switch limit.GetName() {
		case "cpu":
			cpuMilli = q.MilliValue()
		case "memory":
			memBytes = q.Value()
		}
	}
	return cpuMilli, memBytes, nil
}

func schedulingConstraints(actor *ateapipb.Actor, tmpl *ateapipb.ActorTemplate) (scheduling.Constraints, error) {
	// Canonicalized here, the one place an assignment's booked resources are
	// decided, so what is recorded is sorted however the template was authored.
	limits, err := resources.ParseQuantities(tmpl.GetResources())
	if err != nil {
		return scheduling.Constraints{}, fmt.Errorf("invalid template resource limits: %w", err)
	}
	c := scheduling.Constraints{
		SandboxClass:  sandboxClassString(tmpl.GetSandboxConfig().GetSandboxClass()),
		ActorSelector: labels.SelectorFromSet(labels.Set(actor.GetWorkerSelector().GetMatchLabels())),
		RequiredNodes: actor.GetStatus().GetLocalSnapshot().GetNodeVmsWithLocalSnapshots(),
		Limits:        limits.Proto(),
	}
	if sel := tmpl.GetWorkerSelector(); sel != nil {
		c.TemplateSelector = labels.SelectorFromSet(labels.Set(sel.GetMatchLabels()))
	}
	// A SUSPENDED actor restores from its external snapshot; the node that
	// uploaded it may still hold a local copy (atelet retains one), so prefer
	// it. A PAUSED actor is pinned by RequiredNodes instead.
	if actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		if node := actor.GetStatus().GetExternalSnapshot().GetProducedOnNode(); node != "" {
			c.PreferredNodes = []string{node}
		}
	}
	return c, nil
}

// ensureVolumesAttached attaches the actor's mounted external volumes to the
// assigned worker's node. Attachment is idempotent, so a re-entered workflow
// safely runs it again.
// TODO replace re-execution with a proper check on the volumes' attach state.
func (w *ActorWorkflow) ensureVolumesAttached(ctx context.Context, actor *ateapipb.Actor, worker *ateapipb.Worker, actorTemplate *ateapipb.ActorTemplate) (err error) {
	ctx, done := stepSpan(ctx, "AttachVolumes")
	defer func() { err = done(err) }()

	node := worker.GetNodeName()
	if node == "" {
		return fmt.Errorf("assigned worker has no node name")
	}

	ref := &ateapipb.ObjectRef{Atespace: actor.GetMetadata().GetAtespace(), Name: actor.GetMetadata().GetName()}
	for _, vol := range getMountedActorVolumes(ctx, ref, actor.GetStatus().GetActorVolumes(), actorTemplate) {
		slog.InfoContext(ctx, "Attaching volume to node", slog.String("volume_id", vol.GetStorageVolumeId()), slog.String("node", node))
		plugin, err := w.pluginRegistry.GetPlugin(ctx, vol.GetVolumeType())
		if err != nil {
			return fmt.Errorf("failed to get volume plugin for %q: %w", vol.GetVolumeType(), err)
		}
		if err := plugin.AttachVolume(ctx, vol.GetStorageVolumeId(), node); err != nil {
			return fmt.Errorf("failed to attach volume %q to node %q: %w", vol.GetStorageVolumeId(), node, err)
		}
	}
	return nil
}

// ensureAteletRestored brings the workload up on the assigned worker:
// restoring the actor's local snapshot when one exists, else its external
// snapshot, else cold-booting from the template spec. This is the
// atelet reentrancy seam (#372): the request is keyed by the actor UID and
// the worker pod UID, so a re-entered workflow re-sends the same semantic
// request; once atelet's Restore/Run are idempotent on those keys this step
// becomes fully reentrant with no changes here.
func (w *ActorWorkflow) ensureAteletRestored(ctx context.Context, actorRef resources.ActorRef, actor *ateapipb.Actor, actorTemplate *ateapipb.ActorTemplate, src resumeSnapshotSource, tm *resumeTiming) (tele restoreTelemetry, err error) {
	ctx, done := stepSpan(ctx, "CallAteletRestore")
	defer func() { err = done(err) }()

	assignment := actor.GetStatus().GetWorkerAssignment()
	t := time.Now()
	ateletConn, err := w.dialer.DialForAteletOnNode(assignment.GetNodeName())
	tm.ateletDial = time.Since(t)
	if err != nil {
		return tele, err
	}
	client := ateletpb.NewAteomHerderClient(ateletConn)

	workloadSpec, err := workloadSpecFromActorTemplate(actorTemplate, actor)
	if err != nil {
		return tele, err
	}
	egressGateway := w.egressGateway()

	// The actor's declared limits ride the RPC down to the sandbox so it is sized
	// to the actor (replacing the worker-pod downward-API approach).
	cpuMilli, memBytes, err := actorResourceLimits(actorTemplate)
	if err != nil {
		return tele, err
	}

	// The sandbox binaries and pause image come from the template's
	// SandboxConfig on every path, restores included.
	sandboxAssets, err := resolveSandboxAssets(w.sandboxConfigLister, actorTemplate.GetSandboxConfig())
	if err != nil {
		return tele, fmt.Errorf("while resolving sandbox assets: %w", err)
	}

	if local := actor.GetStatus().GetLocalSnapshot(); local != nil {
		slog.InfoContext(ctx, "Actor has snapshot; Restoring from snapshot")
		tele.SnapshotKind = ateattr.SnapshotKindLocal

		req := &ateletpb.RestoreRequest{
			TargetAteomUid:        assignment.GetWorkerPodUid(),
			Atespace:              actor.GetMetadata().GetAtespace(),
			ActorName:             actor.GetMetadata().GetName(),
			ActorTemplateAtespace: actor.GetActorTemplate().GetAtespace(),
			ActorTemplateName:     actor.GetActorTemplate().GetName(),
			Spec:                  workloadSpec,
			SandboxAssets:         sandboxAssets,
			ActorUid:              actor.GetMetadata().Uid,
			EgressGateway:         egressGateway,
			CpuMilli:              cpuMilli,
			MemoryBytes:           memBytes,
		}
		req.Type = ateletpb.CheckpointType_CHECKPOINT_TYPE_LOCAL
		req.Config = &ateletpb.RestoreRequest_LocalConfig{
			LocalConfig: &ateletpb.LocalCheckpointConfiguration{SnapshotName: local.GetSnapshotName()},
		}
		req.Scope = actorSnapshotContentScopeToAtelet(actorTemplate.GetSnapshotConfig().GetOnPause())
		tele.WireSnapshotScope = ateattr.SnapshotScopeValue(req.Scope)

		t = time.Now()
		_, err = client.Restore(ctx, req)
		tm.ateletRestore = time.Since(t)
		if err != nil {
			return tele, handleAteletError(ctx, w.store, actorRef, ateattr.OperationResume, "Restore", false, err)
		}
		return tele, nil
	} else if !src.SnapshotURI.IsZero() {
		slog.InfoContext(ctx, "Actor has durable snapshot; Restoring from snapshot")
		tele.SnapshotKind = ateattr.SnapshotKindLatest
		scope := actorSnapshotContentScopeToAtelet(src.Scope)
		if src.TemplateReplaced {
			scope = ateletpb.SnapshotScope_SNAPSHOT_SCOPE_DATA
		}
		tele.WireSnapshotScope = ateattr.SnapshotScopeValue(scope)
		req := &ateletpb.RestoreRequest{
			TargetAteomUid:        assignment.GetWorkerPodUid(),
			Atespace:              actor.GetMetadata().GetAtespace(),
			ActorName:             actor.GetMetadata().GetName(),
			ActorTemplateAtespace: actor.GetActorTemplate().GetAtespace(),
			ActorTemplateName:     actor.GetActorTemplate().GetName(),
			Spec:                  workloadSpec,
			Type:                  ateletpb.CheckpointType_CHECKPOINT_TYPE_EXTERNAL,
			Config: &ateletpb.RestoreRequest_ExternalConfig{
				ExternalConfig: &ateletpb.ExternalRestoreConfiguration{
					SnapshotUri: src.SnapshotURI.String(),
				},
			},
			Scope:         scope,
			SandboxAssets: sandboxAssets,
			ActorUid:      actor.GetMetadata().Uid,
			EgressGateway: egressGateway,
			CpuMilli:      cpuMilli,
			MemoryBytes:   memBytes,
		}
		t = time.Now()
		_, err = client.Restore(ctx, req)
		tm.ateletRestore = time.Since(t)
		if err != nil {
			return tele, handleAteletError(ctx, w.store, actorRef, ateattr.OperationResume, "Restore", false, err)
		}
		return tele, nil
	} else {
		slog.InfoContext(ctx, "Actor has no snapshot; Booting from ActorTemplate spec")
		tele.SnapshotKind = ateattr.SnapshotKindBoot

		req := &ateletpb.RunRequest{
			TargetAteomUid:        assignment.GetWorkerPodUid(),
			Atespace:              actor.GetMetadata().GetAtespace(),
			ActorName:             actor.GetMetadata().GetName(),
			ActorTemplateAtespace: actor.GetActorTemplate().GetAtespace(),
			ActorTemplateName:     actor.GetActorTemplate().GetName(),
			SandboxAssets:         sandboxAssets,
			Spec:                  workloadSpec,
			ActorUid:              actor.GetMetadata().Uid,
			EgressGateway:         egressGateway,
			CpuMilli:              cpuMilli,
			MemoryBytes:           memBytes,
		}
		// Run is the cold-boot counterpart of Restore; it is timed under the
		// same key.
		t = time.Now()
		_, err = client.Run(ctx, req)
		tm.ateletRestore = time.Since(t)
		if err != nil {
			return tele, handleAteletError(ctx, w.store, actorRef, ateattr.OperationResume, "Run", false, err)
		}
		return tele, nil
	}
}

func (w *ActorWorkflow) egressGateway() *ateletpb.EgressGateway {
	if w.egressGatewayAddress == "" {
		return nil
	}
	return &ateletpb.EgressGateway{Address: w.egressGatewayAddress}
}

// finalizeRunning re-reads the actor for a fresh version and commits RUNNING.
func (w *ActorWorkflow) finalizeRunning(ctx context.Context, actorRef resources.ActorRef) (_ *ateapipb.Actor, err error) {
	ctx, done := stepSpan(ctx, "FinalizeRunning")
	defer func() { err = done(err) }()

	latestActor, err := w.store.GetActor(ctx, actorRef)
	if err != nil {
		return nil, err
	}

	storedActor, err := w.store.UpdateActor(ctx, actorRef, store.PreconditionFrom(latestActor), func(toUpdate *ateapipb.Actor) error {
		toUpdate.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			return nil, status.Error(codes.Aborted, "concurrent update conflict, please retry")
		}
		return nil, err
	}
	logActorStateChanged(ctx, storedActor, ateattr.OperationResume)
	return storedActor, nil
}
