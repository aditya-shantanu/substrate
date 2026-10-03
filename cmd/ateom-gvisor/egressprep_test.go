//go:build linux

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

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/ateomtunnel"
)

// TestEgressPrepJoin: join returns the prep's result and error, and a nil
// gateway's nil egress passes through.
func TestEgressPrepJoin(t *testing.T) {
	t.Parallel()

	want := &ateomtunnel.ActorEgress{}
	p := startEgressPrep(context.Background(), func(context.Context) (*ateomtunnel.ActorEgress, error) {
		return want, nil
	})
	if got, err := p.join(); err != nil || got != want {
		t.Errorf("join() = %v, %v; want %v, nil", got, err, want)
	}
	if p.elapsed < 0 {
		t.Errorf("elapsed = %v, want >= 0", p.elapsed)
	}

	wantErr := errors.New("mint failed")
	p = startEgressPrep(context.Background(), func(context.Context) (*ateomtunnel.ActorEgress, error) {
		return nil, wantErr
	})
	if got, err := p.join(); !errors.Is(err, wantErr) || got != nil {
		t.Errorf("join() = %v, %v; want nil, %v", got, err, wantErr)
	}
}

// TestEgressPrepElapsedIsPrepareWallTime: elapsed measures PrepareEgress
// itself, not how long the caller took to join.
func TestEgressPrepElapsedIsPrepareWallTime(t *testing.T) {
	t.Parallel()

	const work, idle = 20 * time.Millisecond, 50 * time.Millisecond
	t0 := time.Now()
	p := startEgressPrep(context.Background(), func(context.Context) (*ateomtunnel.ActorEgress, error) {
		time.Sleep(work)
		return nil, nil
	})
	<-p.done
	time.Sleep(idle)
	sinceStart := time.Since(t0)
	if _, err := p.join(); err != nil {
		t.Fatalf("join() error = %v", err)
	}
	// The prep finished before the idle sleep began, so its wall time cannot
	// include it.
	if p.elapsed < work || p.elapsed > sinceStart-idle {
		t.Errorf("elapsed = %v, want in [%v, %v]", p.elapsed, work, sinceStart-idle)
	}
}

// TestEgressPrepAbandonCancelsAndWaits: abandon cancels a prep still in
// flight, returns only after it has exited, and is a no-op afterwards.
func TestEgressPrepAbandonCancelsAndWaits(t *testing.T) {
	t.Parallel()

	exited := make(chan struct{})
	p := startEgressPrep(context.Background(), func(ctx context.Context) (*ateomtunnel.ActorEgress, error) {
		defer close(exited)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	select {
	case <-exited:
		t.Fatal("prepare exited before abandon")
	case <-time.After(20 * time.Millisecond):
	}

	p.abandon()
	select {
	case <-exited:
	default:
		t.Fatal("abandon returned before prepare exited")
	}
	if _, err := p.join(); !errors.Is(err, context.Canceled) {
		t.Errorf("join() error = %v, want context.Canceled", err)
	}
	// Idempotent once settled.
	p.abandon()
}

// TestEgressPrepAbandonAfterJoin: abandoning a finished prep keeps its result.
func TestEgressPrepAbandonAfterJoin(t *testing.T) {
	t.Parallel()

	want := &ateomtunnel.ActorEgress{}
	p := startEgressPrep(context.Background(), func(context.Context) (*ateomtunnel.ActorEgress, error) {
		return want, nil
	})
	if _, err := p.join(); err != nil {
		t.Fatalf("join() error = %v", err)
	}
	p.abandon()
	if got, err := p.join(); err != nil || got != want {
		t.Errorf("join() after abandon = %v, %v; want %v, nil", got, err, want)
	}
}
