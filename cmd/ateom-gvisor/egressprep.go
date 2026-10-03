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
	"time"

	"github.com/agent-substrate/substrate/internal/ateomtunnel"
)

// egressPrep is a PrepareEgress running alongside the sandbox setup. The
// certificate mint is a round trip through atelet to ateapi that nothing
// before tunnel.Activate consumes, so it runs off the critical path.
//
// An unactivated result holds only in-memory state (the actor key and the
// gateway client), so an abandoned prep releases nothing.
type egressPrep struct {
	cancel context.CancelFunc
	done   chan struct{}

	egress *ateomtunnel.ActorEgress
	err    error
	// elapsed is PrepareEgress's wall time, valid once done is closed.
	elapsed time.Duration
}

// startEgressPrep runs prepare in a goroutine under a child of ctx.
func startEgressPrep(ctx context.Context, prepare func(context.Context) (*ateomtunnel.ActorEgress, error)) *egressPrep {
	ctx, cancel := context.WithCancel(ctx)
	p := &egressPrep{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		start := time.Now()
		p.egress, p.err = prepare(ctx)
		p.elapsed = time.Since(start)
	}()
	return p
}

// join waits for the prep to finish and returns its result.
func (p *egressPrep) join() (*ateomtunnel.ActorEgress, error) {
	<-p.done
	return p.egress, p.err
}

// abandon cancels a prep still in flight and waits for it to exit, so a
// failure cleanup never overlaps a mint. Instant once the prep has finished.
func (p *egressPrep) abandon() {
	p.cancel()
	<-p.done
}
