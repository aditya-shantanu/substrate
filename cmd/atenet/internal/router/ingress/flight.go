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

package ingress

import (
	"sync/atomic"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// resumeActorFlight encapsulates a single per-actor resume that aggregates
// multiple requests to avoid initiating separate RPCs in parallel for the
// same Actor.
//
// There are two signals for the flight that callers can `select` on:
//
//   - retrying: closed when the flight is retrying. A caller should attempt to
//     enter the parking lot in this state.
//   - done: closed when the flight is finished and result is set.
type resumeActorFlight struct {
	retrying chan struct{}
	// retryingSignaled guards the single close of retrying; only the flight
	// goroutine touches it.
	retryingSignaled bool
	done             chan struct{}
	result           *resumeCallResult

	// attempts and firstAttemptUnixNano are the flight's RPC history, read by
	// callers while the flight is still running (a canceled caller reports what
	// it waited through), hence atomic.
	attempts             atomic.Int32
	firstAttemptUnixNano atomic.Int64

	// spanCtx identifies the flight's own span, the parent of its ResumeActor
	// RPCs. A joiner links its request span to it, which is how a trace of a
	// request that only waited finds the trace that did the resume.
	spanCtx trace.SpanContext
}

func newResumeActorFlight(spanCtx trace.SpanContext) *resumeActorFlight {
	return &resumeActorFlight{retrying: make(chan struct{}), done: make(chan struct{}), spanCtx: spanCtx}
}

// signalRetrying is idempotent; only the flight goroutine calls it.
func (f *resumeActorFlight) signalRetrying() {
	if f.retryingSignaled {
		return
	}
	f.retryingSignaled = true
	close(f.retrying)
}

// noteAttempt records that the flight is starting another ResumeActor RPC.
func (f *resumeActorFlight) noteAttempt() {
	if f.attempts.Add(1) == 1 {
		f.firstAttemptUnixNano.Store(time.Now().UnixNano())
	}
}

// status is one caller's view of f: outcome plus the RPC history so far. Safe
// to call before f.done closes.
func (f *resumeActorFlight) status(outcome ResumeOutcome) ResumeStatus {
	st := ResumeStatus{Outcome: outcome, Attempts: int(f.attempts.Load())}
	if ns := f.firstAttemptUnixNano.Load(); ns != 0 {
		st.FirstAttemptAt = time.Unix(0, ns)
	}
	return st
}

// callerResult classifies f's completed outcome for one caller. It must only
// be called after f.done is closed.
//
// Only a resume that completed without an error says whether an activation
// ran, so the three activation labels are reserved for that case. A failed
// resume reports "unknown": the gRPC code alone does not carry the fact. A
// canceled leader's flight outlives its request and keeps restoring the actor,
// and a DeadlineExceeded can land in the middle of a restore.
func (f *resumeActorFlight) callerResult(reqID uint64) (*ateapipb.Actor, ResumeStatus, error) {
	res := f.result
	if res == nil {
		return nil, f.status(ResumeOutcomeUnknown), status.Error(codes.Internal, "resume call returned nil result")
	}

	if res.err != nil {
		return nil, f.status(ResumeOutcomeUnknown), res.err
	}

	// Disambiguate the shared-flight resume outcome:
	// - ResumeOutcomeNone ("none"): resumed == false, actor was already active/running.
	// - ResumeOutcomeTriggered ("triggered"): Cold activation leader (resumed == true, caller's reqID == leaderID).
	// - ResumeOutcomeJoined ("joined"): Cold activation joiner (resumed == true, caller's reqID != leaderID).
	outcome := ResumeOutcomeNone
	if res.resumed {
		if res.leaderID == reqID {
			outcome = ResumeOutcomeTriggered
		} else {
			outcome = ResumeOutcomeJoined
		}
	}

	return res.actor, f.status(outcome), nil
}

// publish completes f. The order matters: result is written before done closes
// so callers can read it without a lock, and the registry entry is deleted
// before done closes so no caller can attach to a finished flight.
func (r *ActorResumer) publish(f *resumeActorFlight, key string, result *resumeCallResult) {
	f.result = result
	r.mu.Lock()
	delete(r.flights, key)
	r.mu.Unlock()
	close(f.done)
}
