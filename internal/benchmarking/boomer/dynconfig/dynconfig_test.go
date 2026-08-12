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

package dynconfig

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func baseline() Config {
	return Config{
		MinWait:          time.Second,
		MaxWait:          2 * time.Second,
		TraceProbability: 0.5,
		GluttonRAMBytes:  1024,
	}
}

// TestParse covers the merge semantics both Parse and Fetch rely on: a field
// the master did not send must leave the current value alone, while a field
// explicitly set to zero must overwrite it. The distinction matters because
// zero is a meaningful value for every knob here — it means "no wait", "never
// trace", "no RAM write".
func TestParse(t *testing.T) {
	tests := []struct {
		name string
		json string
		want Config
	}{
		{
			name: "empty input keeps everything",
			json: "",
			want: baseline(),
		},
		{
			name: "absent fields keep their current values",
			json: `{}`,
			want: baseline(),
		},
		{
			name: "glutton_ram_bytes is applied",
			json: `{"glutton_ram_bytes": 268435456}`,
			want: Config{MinWait: time.Second, MaxWait: 2 * time.Second, TraceProbability: 0.5, GluttonRAMBytes: 268435456},
		},
		{
			name: "explicit zero disables the RAM write",
			json: `{"glutton_ram_bytes": 0}`,
			want: Config{MinWait: time.Second, MaxWait: 2 * time.Second, TraceProbability: 0.5, GluttonRAMBytes: 0},
		},
		{
			name: "fractional bytes truncate to an integer count",
			json: `{"glutton_ram_bytes": 1536.9}`,
			want: Config{MinWait: time.Second, MaxWait: 2 * time.Second, TraceProbability: 0.5, GluttonRAMBytes: 1536},
		},
		{
			name: "all fields together",
			json: `{"trace_probability": 0.1, "min_wait_time": 0.25, "max_wait_time": 3, "glutton_ram_bytes": 4096}`,
			want: Config{MinWait: 250 * time.Millisecond, MaxWait: 3 * time.Second, TraceProbability: 0.1, GluttonRAMBytes: 4096},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse([]byte(tc.json), baseline())
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.json, err)
			}
			if got != tc.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tc.json, got, tc.want)
			}
		})
	}
}

func TestParseRejectsMalformedJSON(t *testing.T) {
	got, err := Parse([]byte(`{"glutton_ram_bytes":`), baseline())
	if err == nil {
		t.Fatal("Parse(malformed) = nil error, want an error")
	}
	if got != baseline() {
		t.Errorf("Parse(malformed) = %+v, want the config left untouched", got)
	}
}

// TestFetch pins the wire contract with common/boomer_config.py's
// /boomer-config route, which serves every flag in _FLAGS on every request.
func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"trace_probability": 0.2, "min_wait_time": 1, "max_wait_time": 1, "glutton_ram_bytes": 268435456}`))
	}))
	defer srv.Close()

	got, err := Fetch(context.Background(), srv.URL, Config{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := Config{MinWait: time.Second, MaxWait: time.Second, TraceProbability: 0.2, GluttonRAMBytes: 268435456}
	if got != want {
		t.Errorf("Fetch = %+v, want %+v", got, want)
	}
}

// TestFetchNullFieldsKeepCurrent guards the real shape of a /boomer-config
// response when the operator never set a flag: the route emits an explicit
// null rather than omitting the key, and null must be treated as "unset".
func TestFetchNullFieldsKeepCurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"trace_probability": null, "min_wait_time": null, "max_wait_time": null, "glutton_ram_bytes": null}`))
	}))
	defer srv.Close()

	got, err := Fetch(context.Background(), srv.URL, baseline())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != baseline() {
		t.Errorf("Fetch with null fields = %+v, want %+v", got, baseline())
	}
}

func TestFetchErrorKeepsCurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	got, err := Fetch(context.Background(), srv.URL, baseline())
	if err == nil {
		t.Fatal("Fetch(500) = nil error, want an error")
	}
	if got != baseline() {
		t.Errorf("Fetch(500) = %+v, want the config left untouched", got)
	}
}

func TestHolderRoundTrip(t *testing.T) {
	h := NewHolder(baseline())
	if got := h.Load(); got != baseline() {
		t.Errorf("Load = %+v, want %+v", got, baseline())
	}
	next := baseline()
	next.GluttonRAMBytes = 42
	h.Store(next)
	if got := h.Load(); got != next {
		t.Errorf("Load after Store = %+v, want %+v", got, next)
	}
}
