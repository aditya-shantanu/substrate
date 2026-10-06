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

// Package phasespan turns the phases of a snapshot operation into trace
// spans. atelet and the ateoms already time their restore and checkpoint
// phases for a log record and a histogram; a span per phase puts the same
// partition on the request's trace, where it lines up with the RPC spans
// above it and the GCS and runsc work below it.
//
// Spans are named "<op>.<phase>" (restore.download, checkpoint.persist) and
// are children of the span in the context they are opened from, siblings of
// one another rather than nested, so each span's duration is that phase alone.
package phasespan

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Start opens the span "<op>.<name>" under ctx and returns the span's context
// and a finish func. A non-nil error passed to finish is recorded on the
// span. Use it for a phase that runs concurrently with others; sequential
// phases read better through a Sequence.
func Start(ctx context.Context, tracer trace.Tracer, op, name string, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	ctx, span := tracer.Start(ctx, op+"."+name, trace.WithAttributes(attrs...))
	return ctx, func(err error) { endSpan(span, err) }
}

func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// Sequence opens one span per phase of a sequential operation. Next ends the
// phase in progress and opens the following one; End closes the last. Every
// phase span is a child of the context the Sequence was made from.
type Sequence struct {
	tracer trace.Tracer
	parent context.Context
	op     string
	cur    trace.Span
}

// NewSequence returns a Sequence for operation op whose phase spans hang off
// the span in ctx. No span is open until the first Next.
func NewSequence(ctx context.Context, tracer trace.Tracer, op string) *Sequence {
	return &Sequence{tracer: tracer, parent: ctx, op: op}
}

// Next ends the current phase, if any, as successful and opens "<op>.<name>".
// It returns the new phase's context for work that opens spans of its own.
func (s *Sequence) Next(name string, attrs ...attribute.KeyValue) context.Context {
	s.End(nil)
	ctx, span := s.tracer.Start(s.parent, s.op+"."+name, trace.WithAttributes(attrs...))
	s.cur = span
	return ctx
}

// End closes the current phase, recording err on it when non-nil. Safe to call
// with no phase open and safe to call twice.
func (s *Sequence) End(err error) {
	if s.cur == nil {
		return
	}
	endSpan(s.cur, err)
	s.cur = nil
}
