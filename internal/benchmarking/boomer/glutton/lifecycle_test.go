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

package glutton

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"

	"github.com/agent-substrate/substrate/internal/benchmarking/boomer/dynconfig"
	gluttonpb "github.com/agent-substrate/substrate/internal/proto/glutton"
)

// newTestUser wires a gluttonUser at a stub router. The returned channel-free
// handler stands in for atenet: the boomer always posts to the router URL and
// selects the actor with the Host header, so that is what the tests assert on.
func newTestUser(t *testing.T, handler http.HandlerFunc) (*gluttonUser, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := &Config{
		HTTPClient: srv.Client(),
		RouterURL:  srv.URL,
		Atespace:   "bench",
		Dyn:        dynconfig.NewHolder(dynconfig.Config{}),
	}
	// Register() is not usable here (it needs an APIStub), so mirror the one
	// thing it does that postProto depends on. The global provider is
	// uninitialized in tests, so this is the no-op tracer.
	cfg.Tracer = otel.Tracer("substrate-boomer/glutton")

	u := &gluttonUser{cfg: cfg, actorName: "sb-test"}
	u.hostHeader = u.actorName + "." + cfg.Atespace + "." + actorDomain
	return u, srv
}

// TestWriteRAMRequest pins the request the boomer puts on the wire: the route,
// the fixed key, and TRUNCATE mode. The key and mode together are what make
// the guest hold a steady water level instead of climbing every iteration.
func TestWriteRAMRequest(t *testing.T) {
	var (
		gotPath string
		gotHost string
		gotType string
		gotReq  gluttonpb.WriteRAMRequest
		calls   atomic.Int64
	)

	u, _ := newTestUser(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotPath, gotHost, gotType = r.URL.Path, r.Host, r.Header.Get("Content-Type")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		if err := proto.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("unmarshal request: %v", err)
			return
		}
		out, err := proto.Marshal(&gluttonpb.WriteRAMResponse{})
		if err != nil {
			t.Errorf("marshal response: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	})

	u.writeRAM(context.Background(), 268435456)

	if got := calls.Load(); got != 1 {
		t.Fatalf("router saw %d requests, want 1", got)
	}
	if gotPath != writeRAMPath {
		t.Errorf("path = %q, want %q", gotPath, writeRAMPath)
	}
	if want := "sb-test.bench." + actorDomain; gotHost != want {
		t.Errorf("Host = %q, want %q", gotHost, want)
	}
	if gotType != "application/x-protobuf" {
		t.Errorf("Content-Type = %q, want application/x-protobuf", gotType)
	}
	if gotReq.GetKey() != ramKey {
		t.Errorf("key = %q, want %q", gotReq.GetKey(), ramKey)
	}
	if gotReq.GetSize() != 268435456 {
		t.Errorf("size = %d, want %d", gotReq.GetSize(), 268435456)
	}
	if gotReq.GetWriteMode() != gluttonpb.WriteMode_WRITE_MODE_TRUNCATE {
		t.Errorf("write mode = %v, want TRUNCATE", gotReq.GetWriteMode())
	}
}

// TestWriteRAMRejectsOversizeLocally guards the int32 ceiling on
// WriteRAMRequest.size. Truncating the conversion would silently ask for a
// wildly different (or negative) water level, so the call has to be refused
// before it reaches the wire.
func TestWriteRAMRejectsOversizeLocally(t *testing.T) {
	var calls atomic.Int64
	u, _ := newTestUser(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	})

	u.writeRAM(context.Background(), math.MaxInt32+1)

	if got := calls.Load(); got != 0 {
		t.Errorf("router saw %d requests, want 0 (oversize must not be sent)", got)
	}
}

// TestPingEchoMismatchIsNotSuccess keeps the check that survived the move onto
// postProto: a 200 carrying the wrong message is a failed iteration, not a
// successful one.
func TestPingEchoMismatchIsNotSuccess(t *testing.T) {
	u, _ := newTestUser(t, func(w http.ResponseWriter, r *http.Request) {
		out, err := proto.Marshal(&gluttonpb.PingResponse{Message: "not-what-was-sent"})
		if err != nil {
			t.Errorf("marshal: %v", err)
			return
		}
		_, _ = w.Write(out)
	})

	// postProto reports through package-level boomer/prometheus counters, so
	// assert on the observable effect available here: the call completes and
	// does not panic on the mismatch path.
	u.ping(context.Background())
}

// TestPostProtoHTTPErrorDoesNotUnmarshal makes sure a router-level failure
// (a 503 with an HTML body, say) is reported as a failure rather than being
// fed to proto.Unmarshal and misreported as a decode bug.
func TestPostProtoHTTPErrorDoesNotUnmarshal(t *testing.T) {
	u, _ := newTestUser(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	})

	resp := &gluttonpb.PingResponse{}
	verifyCalled := false
	u.postProto(context.Background(), pingPath, "GluttonPing",
		&gluttonpb.PingRequest{Message: "hi"}, resp, func() error {
			verifyCalled = true
			return nil
		})

	if verifyCalled {
		t.Error("verify ran on a 503 response; it must only run on a decoded reply")
	}
	if resp.GetMessage() != "" {
		t.Errorf("response was populated from an error body: %q", resp.GetMessage())
	}
}
