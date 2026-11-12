//go:build linux

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

package main

import (
	"context"
	"fmt"
	"log/slog"

	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/ateattr"
	"go.ate.dev/substrate/internal/ateworkervalidation"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/workerstats"
)

const rpcAdoptWorkload = "AdoptWorkload"

// AdoptWorkload implements ateworkerpb.Worker/AdoptWorkload. The VM, its
// network namespace, tap, cgroup, and virtio-fs shares keep running; they are
// all keyed by the actor's UID, which does not change. The host state keyed
// by the actor's name moves to the new name: ingress, the DNS egress policy
// source, the egress certificate, and the attribution on stats, crash reports,
// and lifecycle logs. The guest's hostname is a constant, and the system-info
// share is a bind of the host directory atelet rewrites. The containers'
// stdio log forwarders keep the name they were started with.
func (s *AteWorkerService) AdoptWorkload(ctx context.Context, req *ateworkerpb.AdoptWorkloadRequest) (*ateworkerpb.AdoptWorkloadResponse, error) {
	if err := ateworkervalidation.ValidateAdoptWorkloadRequest(ctx, req); err != nil {
		return nil, err
	}
	epoch, err := s.epoch.Require()
	if err != nil {
		return nil, err
	}
	uid := req.GetActorUid()
	if !s.locks.Lock(ctx, uid) {
		return nil, fmt.Errorf("gave up waiting for the actor's lock: %w", ctx.Err())
	}
	defer s.locks.Unlock(uid)
	release, err := s.beginRPC(uid, rpcAdoptWorkload, nil)
	if err != nil {
		return nil, err
	}
	defer release()

	hosted := s.lookupActor(uid)
	if hosted == nil {
		return nil, apierror.NotFound("worker is not executing actor %q", uid)
	}
	previous := hosted.attribution()
	adopted := workerstats.ActorAttributionFromRequest(req)
	if previous.Ref == adopted.Ref {
		// A retry of an adopt that completed.
		return &ateworkerpb.AdoptWorkloadResponse{WorkerEpoch: epoch}, nil
	}
	if previous.Ref.Atespace != adopted.Ref.Atespace || previous.Ref.Name != req.GetPreviousActorName() {
		return nil, apierror.FailedPrecondition("worker hosts actor %q as %s, not as %s/%s", uid, previous.Ref, req.GetAtespace(), req.GetPreviousActorName())
	}

	// Ingress and the policy source change locally and are put back if the
	// mint for the new name fails.
	dial := s.sandboxDialer(uid)
	if err := s.tunnel.AdoptIngress(ctx, previous.Ref, adopted, dial); err != nil {
		return nil, err
	}
	hosted.dnsPolicy.Retarget(adopted)
	if err := s.tunnel.AdoptEgress(ctx, adopted, req.GetEgressGateway()); err != nil {
		hosted.dnsPolicy.Retarget(previous)
		if err := s.tunnel.AdoptIngress(ctx, adopted.Ref, previous, dial); err != nil {
			attrs := append(ateattr.ActorLogAttrs(previous), slog.Any("err", err))
			slog.LogAttrs(ctx, slog.LevelWarn, "Failed to restore the actor's ingress after a failed adopt", attrs...)
		}
		return nil, err
	}
	hosted.adopt(adopted)

	attrs := append(ateattr.ActorLogAttrs(adopted), slog.String("previousActorName", previous.Ref.Name))
	slog.LogAttrs(ctx, slog.LevelInfo, "Actor adopted", attrs...)
	s.actorLogger.EmitLifecycleLog(ctx, "Actor adopted", adopted)
	return &ateworkerpb.AdoptWorkloadResponse{WorkerEpoch: epoch}, nil
}
