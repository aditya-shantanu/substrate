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
	"errors"
	"log/slog"
	"os"

	"go.ate.dev/substrate/cmd/atelet/internal/apivalidation"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/proto/ateletpb"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Adopt implements ateletpb.Atelet/Adopt. The actor's directories, snapshots,
// and sandbox record are keyed by UID and stay as they are. atelet writes the
// system-info volumes for the new name, drops the actor's cached JWT, and has
// the worker move its name-keyed state. A later Checkpoint or Terminate names
// the actor as its request does.
func (s *Atelet) Adopt(ctx context.Context, req *ateletpb.AdoptRequest) (*ateletpb.AdoptResponse, error) {
	if errs := apivalidation.ValidateAdoptRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}
	actorUID := req.GetActorUid()
	actorRef := resources.ActorRef{Atespace: req.GetAtespace(), Name: req.GetActorName()}
	previous := resources.ActorRef{Atespace: req.GetAtespace(), Name: req.GetPreviousActorName()}

	unlockActor, err := s.lockActor(ctx, actorUID)
	if err != nil {
		return nil, err
	}
	defer unlockActor()

	// Run and Restore write the record before starting the workload, and
	// Terminate removes it. Without it, no sandbox runs for the actor here.
	if _, err := readSandboxRecord(actorUID); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, apierror.NotFound("no sandbox was started for actor %q on this node", actorUID)
		}
		return nil, err
	}
	client, err := s.dialWorker(ctx, req.GetWorkerPodUid())
	if err != nil {
		return nil, err
	}

	registered, err := s.systemInfoVolumes.Adopt(actorUID, actorRef)
	if err != nil {
		return nil, err
	}
	if !registered {
		slog.WarnContext(ctx, "Actor has no system-info volume registration to adopt; its volumes keep the previous name",
			slog.Any("actor", actorRef), slog.Any("previous", previous), slog.String("actorUID", actorUID))
	}
	if s.workerActorJWTs != nil {
		s.workerActorJWTs.forget(actorUID)
	}

	resp, err := client.AdoptWorkload(ctx, &ateworkerpb.AdoptWorkloadRequest{
		Atespace:              actorRef.Atespace,
		ActorUid:              actorUID,
		PreviousActorName:     previous.Name,
		ActorName:             actorRef.Name,
		ActorTemplateAtespace: req.GetActorTemplateAtespace(),
		ActorTemplateName:     req.GetActorTemplateName(),
		EgressGateway:         toWorkerEgressGateway(req.GetEgressGateway()),
	})
	if err != nil {
		if registered {
			if _, err := s.systemInfoVolumes.Adopt(actorUID, previous); err != nil {
				slog.WarnContext(ctx, "Failed to restore the actor's system-info volumes after a failed adopt",
					slog.Any("actor", previous), slog.String("actorUID", actorUID), slog.Any("err", err))
			}
		}
		return nil, workerAdoptError(err)
	}
	return &ateletpb.AdoptResponse{WorkerEpoch: resp.GetWorkerEpoch()}, nil
}

// workerAdoptError reports a failed worker AdoptWorkload with the codes the
// control plane acts on: Unimplemented, for a runtime that restores instead,
// NotFound and FailedPrecondition, when the worker does not host the actor
// under the previous name, and Unavailable, for a retry. Anything else is
// Internal.
func workerAdoptError(err error) error {
	switch st := status.Convert(err); st.Code() {
	case codes.Unimplemented:
		return apierror.Unimplemented("worker AdoptWorkload: %s", st.Message())
	case codes.NotFound:
		return apierror.NotFound("worker AdoptWorkload: %s", st.Message())
	case codes.FailedPrecondition:
		return apierror.FailedPrecondition("worker AdoptWorkload: %s", st.Message())
	}
	return workerCallError("AdoptWorkload", err)
}
