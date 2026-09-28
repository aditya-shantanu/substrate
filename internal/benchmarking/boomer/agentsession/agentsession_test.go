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

package agentsession

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/benchmarking/boomer/dynconfig"
	"github.com/agent-substrate/substrate/internal/benchmarking/boomer/userclass"
	"github.com/agent-substrate/substrate/internal/benchmarking/glutton/fake"
)

// TestSessionScriptIsWellFormed pins the script's invariants: unique step
// names, positive think times, and no op that consumes a sandbox object
// before an earlier step created it. A broken ordering would fail at run
// time with NotFound from glutton; this catches it at test time.
func TestSessionScriptIsWellFormed(t *testing.T) {
	steps := Session()
	if len(steps) != 20 {
		t.Fatalf("Session has %d steps, want 20", len(steps))
	}

	seen := map[string]bool{}
	ramFilled := map[string]bool{}
	diskWritten := map[string]bool{}

	for i, s := range steps {
		if s.Name == "" || s.Agent == "" {
			t.Errorf("step %d: Name and Agent must be set", i)
		}
		if seen[s.Name] {
			t.Errorf("step %q: duplicate name", s.Name)
		}
		seen[s.Name] = true
		if s.Think <= 0 {
			t.Errorf("step %q: think time must be positive", s.Name)
		}
		if len(s.Ops) == 0 {
			t.Errorf("step %q: has no ops", s.Name)
		}
		for _, o := range s.Ops {
			switch o.kind {
			case opFillRAM:
				ramFilled[o.key] = true
			case opChurnRAM, opWalkRAM:
				if !ramFilled[o.key] {
					t.Errorf("step %q: %s RAM op before any fill of %q", s.Name, opName(o.kind), o.key)
				}
			case opIngest, opWriteDisk:
				diskWritten[o.key] = true
			case opReadDiskDigest, opReadDiskData:
				if !diskWritten[o.key] {
					t.Errorf("step %q: read of %q before any write", s.Name, o.key)
				}
			}
		}
	}
}

func opName(k opKind) string {
	switch k {
	case opChurnRAM:
		return "churn"
	case opWalkRAM:
		return "walk"
	default:
		return "op"
	}
}

// TestSessionBudgets bounds the bytes the script itself declares: resident
// RAM (largest fill per key) and disk (largest object per key). It does NOT
// model the guest's real peak — kernel, kata-agent, and allocator transients
// sit on top — so the budgets are deliberately far below the 1Gi the
// template calls for. A script edit that outgrows them must come with a
// fresh look at the actor memory guidance.
func TestSessionBudgets(t *testing.T) {
	const (
		ramBudget  = 128 << 20 // bytes
		diskBudget = 256 << 20
	)
	ramMax := map[string]int64{}
	diskMax := map[string]int64{}
	for _, s := range Session() {
		for _, o := range s.Ops {
			switch o.kind {
			case opFillRAM:
				if o.bytes > ramMax[o.key] {
					ramMax[o.key] = o.bytes
				}
			case opIngest, opWriteDisk:
				if o.bytes > diskMax[o.key] {
					diskMax[o.key] = o.bytes
				}
			}
		}
	}
	var ramTotal, diskTotal int64
	for _, v := range ramMax {
		ramTotal += v
	}
	for _, v := range diskMax {
		diskTotal += v
	}
	if ramTotal > ramBudget {
		t.Errorf("script fills %d bytes of RAM, budget %d", ramTotal, ramBudget)
	}
	if diskTotal > diskBudget {
		t.Errorf("script writes %d bytes of disk, budget %d", diskTotal, diskBudget)
	}
}

// TestExecOpAgainstFake replays every scripted op against the fake glutton
// server, proving each op marshals a request the actor-side routes accept.
func TestExecOpAgainstFake(t *testing.T) {
	fakeSrv := &fake.Server{Data: []byte("filecontents")}
	ts := fakeSrv.Start(t)

	u := &sessionUser{
		cfg: &userclass.Config{
			HTTPClient: http.DefaultClient,
			RouterURL:  ts.URL,
			Atespace:   "benchmark",
			Dyn:        dynconfig.NewHolder(dynconfig.Config{}),
		},
		actorName: "agent-test",
	}

	var opCount int
	for _, s := range Session() {
		for i, o := range s.Ops {
			// Cap ingest payloads in the unit test: transport shape is what
			// matters here, not moving tens of MiB through httptest.
			if o.kind == opIngest && o.bytes > 1<<10 {
				o.bytes = 1 << 10
			}
			if o.kind == opBurnCPU {
				o.millis = 1
			}
			if err := u.execOp(context.Background(), o); err != nil {
				t.Fatalf("step %q op %d: %v", s.Name, i, err)
			}
			opCount++
		}
	}
	if got := len(fakeSrv.RecordedPaths()); got != opCount {
		t.Errorf("fake served %d requests, want %d", got, opCount)
	}
	for _, n := range fakeSrv.RecordedIngestSizes() {
		if n > 1<<10 {
			t.Errorf("ingest payload of %d bytes reached the server, cap is %d", n, 1<<10)
		}
	}
	for _, ms := range fakeSrv.RecordedBurnMillis() {
		if ms != 1 {
			t.Errorf("burn of %dms reached the server, override is 1ms", ms)
		}
	}
}

// TestThinkScaling checks the think-time multiplier and its jitter bounds.
func TestThinkScaling(t *testing.T) {
	r := &runtime{
		cfg: &userclass.Config{
			Dyn: dynconfig.NewHolder(dynconfig.Config{AgentSessionThinkScale: 0.5}),
		},
	}
	s := Step{Think: 10 * time.Second}
	for i := 0; i < 100; i++ {
		got := r.think(s)
		if got < 4*time.Second || got > 6*time.Second {
			t.Fatalf("think = %v, want within [4s, 6s] (scale 0.5, jitter ±20%%)", got)
		}
	}

	// Zero scale reads as 1.0.
	r.cfg.Dyn.Store(dynconfig.Config{})
	for i := 0; i < 100; i++ {
		got := r.think(s)
		if got < 8*time.Second || got > 12*time.Second {
			t.Fatalf("think = %v with unset scale, want within [8s, 12s]", got)
		}
	}
}
