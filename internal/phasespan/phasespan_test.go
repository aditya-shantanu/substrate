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

package phasespan

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func recorder(t *testing.T) (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return sr, tp
}

func TestSequencePhasesAreSiblingsOfTheParent(t *testing.T) {
	sr, tp := recorder(t)
	tracer := tp.Tracer("test")
	ctx, root := tracer.Start(context.Background(), "op")

	seq := NewSequence(ctx, tracer, "restore")
	seq.Next("prep")
	seq.Next("download")
	seq.End(errors.New("boom"))
	seq.End(nil) // idempotent
	root.End()

	ended := sr.Ended()
	if len(ended) != 3 {
		t.Fatalf("got %d spans, want 3", len(ended))
	}
	names := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range ended {
		names[s.Name()] = s
	}
	for _, name := range []string{"restore.prep", "restore.download"} {
		s, ok := names[name]
		if !ok {
			t.Fatalf("missing span %s", name)
		}
		if s.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("%s parent = %s, want the root span", name, s.Parent().SpanID())
		}
	}
	if names["restore.prep"].Status().Code != codes.Unset {
		t.Errorf("prep status = %v, want unset", names["restore.prep"].Status())
	}
	if names["restore.download"].Status().Code != codes.Error {
		t.Errorf("download status = %v, want error", names["restore.download"].Status())
	}
	if !names["restore.prep"].EndTime().Before(names["restore.download"].StartTime().Add(1)) {
		t.Errorf("prep should end before download starts")
	}
}

func TestStartNestsUnderThePhaseContext(t *testing.T) {
	sr, tp := recorder(t)
	tracer := tp.Tracer("test")
	ctx, root := tracer.Start(context.Background(), "op")

	seq := NewSequence(ctx, tracer, "checkpoint")
	phaseCtx := seq.Next("persist")
	_, end := Start(phaseCtx, tracer, "checkpoint", "upload")
	end(nil)
	seq.End(nil)
	root.End()

	var upload, persist sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		switch s.Name() {
		case "checkpoint.upload":
			upload = s
		case "checkpoint.persist":
			persist = s
		}
	}
	if upload == nil || persist == nil {
		t.Fatalf("missing spans: upload=%v persist=%v", upload != nil, persist != nil)
	}
	if upload.Parent().SpanID() != persist.SpanContext().SpanID() {
		t.Errorf("upload parent = %s, want persist %s", upload.Parent().SpanID(), persist.SpanContext().SpanID())
	}
}
