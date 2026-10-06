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
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func wakeTestActor(state ateapipb.ActorState, snap *ateapipb.ExternalSnapshot) *ateapipb.Actor {
	return &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor-1", Uid: "uid-1"},
		Status:   &ateapipb.ActorStatus{State: state, ExternalSnapshot: snap},
	}
}

func installRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	return sr
}

func linkKinds(s sdktrace.ReadOnlySpan) map[string]trace.SpanContext {
	out := map[string]trace.SpanContext{}
	for _, l := range s.Links() {
		for _, kv := range l.Attributes {
			if kv.Key == "ate.link.kind" {
				out[kv.Value.AsString()] = l.SpanContext
			}
		}
	}
	return out
}

func TestStartWake_RootsALinkedTraceUnderTraceEveryWake(t *testing.T) {
	sr := installRecorder(t)
	prev := TraceEveryWake
	TraceEveryWake = true
	t.Cleanup(func() { TraceEveryWake = prev })

	reqCtx, reqSpan := otel.Tracer("test").Start(context.Background(), "ateapi.Control/ResumeActor")
	snap := &ateapipb.ExternalSnapshot{SnapshotUri: "gs://b/p", ProducedByTraceId: "0af7651916cd43dd8448eb211c80319c", ProducedBySpanId: "b7ad6b7169203331"}
	wakeCtx, wakeSpan := startWake(reqCtx, wakeTestActor(ateapipb.ActorState_ACTOR_STATE_SUSPENDED, snap))
	wakeSpan.End()
	reqSpan.End()

	if got, want := wakeSpan.SpanContext().TraceID(), reqSpan.SpanContext().TraceID(); got == want {
		t.Fatalf("wake span shares the request's trace %s; want a new root", got)
	}
	if !trace.SpanFromContext(wakeCtx).SpanContext().Equal(wakeSpan.SpanContext()) {
		t.Error("returned context does not carry the wake span")
	}
	var wake, req sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		switch s.Name() {
		case WakeSpanName:
			wake = s
		case "ateapi.Control/ResumeActor":
			req = s
		}
	}
	if wake == nil || req == nil {
		t.Fatalf("spans missing: wake=%v req=%v", wake != nil, req != nil)
	}
	if wake.Parent().IsValid() {
		t.Errorf("wake span has parent %s; want none", wake.Parent().SpanID())
	}
	wl := linkKinds(wake)
	if wl["request"].SpanID() != reqSpan.SpanContext().SpanID() {
		t.Errorf("wake -> request link = %v, want %s", wl["request"], reqSpan.SpanContext().SpanID())
	}
	if wl["snapshot_producer"].TraceID().String() != snap.ProducedByTraceId || wl["snapshot_producer"].SpanID().String() != snap.ProducedBySpanId {
		t.Errorf("wake -> snapshot producer link = %v, want %s/%s", wl["snapshot_producer"], snap.ProducedByTraceId, snap.ProducedBySpanId)
	}
	rl := linkKinds(req)
	if rl["wake"].SpanID() != wakeSpan.SpanContext().SpanID() {
		t.Errorf("request -> wake link = %v, want %s", rl["wake"], wakeSpan.SpanContext().SpanID())
	}
	var gotTraceID string
	for _, kv := range req.Attributes() {
		if string(kv.Key) == wakeTraceIDKey {
			gotTraceID = kv.Value.AsString()
		}
	}
	if gotTraceID != wakeSpan.SpanContext().TraceID().String() {
		t.Errorf("request %s = %q, want %s", wakeTraceIDKey, gotTraceID, wakeSpan.SpanContext().TraceID())
	}
}

func TestStartWake_NestsUnderTheRequestForAnUnwakeableState(t *testing.T) {
	installRecorder(t)
	prev := TraceEveryWake
	TraceEveryWake = true
	t.Cleanup(func() { TraceEveryWake = prev })

	reqCtx, reqSpan := otel.Tracer("test").Start(context.Background(), "ateapi.Control/ResumeActor")
	_, wakeSpan := startWake(reqCtx, wakeTestActor(ateapipb.ActorState_ACTOR_STATE_CRASHED, nil))
	wakeSpan.End()
	reqSpan.End()
	if wakeSpan.SpanContext().TraceID() != reqSpan.SpanContext().TraceID() {
		t.Errorf("crashed actor: wake span trace %s, want the request's %s", wakeSpan.SpanContext().TraceID(), reqSpan.SpanContext().TraceID())
	}
}

func TestStartWake_NestsUnderTheRequestWhenOff(t *testing.T) {
	installRecorder(t)
	prev := TraceEveryWake
	TraceEveryWake = false
	t.Cleanup(func() { TraceEveryWake = prev })

	reqCtx, reqSpan := otel.Tracer("test").Start(context.Background(), "ateapi.Control/ResumeActor")
	// A malformed producer id must not break the wake: no link, no error.
	snap := &ateapipb.ExternalSnapshot{SnapshotUri: "gs://b/p", ProducedByTraceId: "not-hex", ProducedBySpanId: "x"}
	_, wakeSpan := startWake(reqCtx, wakeTestActor(ateapipb.ActorState_ACTOR_STATE_SUSPENDED, snap))
	wakeSpan.End()
	reqSpan.End()

	if wakeSpan.SpanContext().TraceID() != reqSpan.SpanContext().TraceID() {
		t.Errorf("wake span trace %s, want the request's %s", wakeSpan.SpanContext().TraceID(), reqSpan.SpanContext().TraceID())
	}
	if n := len(wakeSpan.(sdktrace.ReadOnlySpan).Links()); n != 0 {
		t.Errorf("wake span has %d links, want none", n)
	}
}

func TestProducerIDs(t *testing.T) {
	if tid, sid := producerIDs(context.Background()); tid != "" || sid != "" {
		t.Errorf("no span: got %q/%q, want empty", tid, sid)
	}
	installRecorder(t)
	ctx, span := otel.Tracer("test").Start(context.Background(), "suspend")
	defer span.End()
	tid, sid := producerIDs(ctx)
	if tid != span.SpanContext().TraceID().String() || sid != span.SpanContext().SpanID().String() {
		t.Errorf("got %s/%s, want %s/%s", tid, sid, span.SpanContext().TraceID(), span.SpanContext().SpanID())
	}
	if _, ok := snapshotProducerLink(&ateapipb.ExternalSnapshot{ProducedByTraceId: tid, ProducedBySpanId: sid}); !ok {
		t.Error("valid ids should produce a link")
	}
}
