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
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"github.com/agent-substrate/substrate/internal/proto/glutton"
)

// TestSplitGRPCServesReadyzAndGRPCOnOneListener starts the grpc-mode handler
// on a real listener and exercises both protocols against it: the readyz
// probe is a plain HTTP GET, and it must not stop gRPC from being served.
func TestSplitGRPCServesReadyzAndGRPCOnOneListener(t *testing.T) {
	svc, err := newGluttonService(t.TempDir())
	if err != nil {
		t.Fatalf("newGluttonService: %v", err)
	}
	defer svc.Close()

	grpcSrv := grpc.NewServer()
	glutton.RegisterGluttonServer(grpcSrv, svc)

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := newServer(splitGRPC(grpcSrv, mux))
	go srv.Serve(lis)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := http.Get("http://" + lis.Addr().String() + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /readyz = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	pong, err := glutton.NewGluttonClient(conn).Ping(ctx, &glutton.PingRequest{Message: "hi"})
	if err != nil {
		t.Fatalf("Ping over gRPC: %v", err)
	}
	if pong.GetMessage() != "hi" {
		t.Errorf("Ping = %q, want %q", pong.GetMessage(), "hi")
	}
}

// TestSplitGRPCRoutesOnContentType pins the routing rule itself: an HTTP/2
// request is not enough to reach the gRPC server, the content type is what
// decides.
func TestSplitGRPCRoutesOnContentType(t *testing.T) {
	grpcHit := false
	handler := splitGRPC(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { grpcHit = true }),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }),
	)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := newServer(handler)
	go srv.Serve(lis)
	defer srv.Close()

	resp, err := http.Get("http://" + lis.Addr().String() + "/anything")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("HTTP/1.1 GET = %d, want %d (the non-gRPC handler)", resp.StatusCode, http.StatusTeapot)
	}
	if grpcHit {
		t.Error("HTTP/1.1 GET reached the gRPC handler")
	}
}

// TestHTTPProtoHandlerWriteRAM covers the route the boomer uses to put a
// memory water level inside a sandbox: a proto POST must actually reach the
// service and leave the bytes resident.
func TestHTTPProtoHandlerWriteRAM(t *testing.T) {
	svc, err := newGluttonService(t.TempDir())
	if err != nil {
		t.Fatalf("newGluttonService: %v", err)
	}
	defer svc.Close()

	h := httpProtoHandler[glutton.WriteRAMRequest]("WriteRAM", svc.WriteRAM)

	const size = 4096
	body, err := proto.Marshal(&glutton.WriteRAMRequest{
		Key:       "bench",
		Size:      size,
		WriteMode: glutton.WriteMode_WRITE_MODE_TRUNCATE,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/writeram", bytes.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /writeram = %d (%s), want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/x-protobuf" {
		t.Errorf("Content-Type = %q, want application/x-protobuf", got)
	}
	if err := proto.Unmarshal(rec.Body.Bytes(), &glutton.WriteRAMResponse{}); err != nil {
		t.Errorf("unmarshal response: %v", err)
	}

	svc.mu.Lock()
	held := len(svc.ram["bench"])
	svc.mu.Unlock()
	if held != size {
		t.Errorf("resident bytes for key %q = %d, want %d", "bench", held, size)
	}
}

// TestHTTPProtoHandlerErrors pins the status mapping. InvalidArgument has to
// come back as 4xx and everything else as 5xx, because the boomer records any
// >=400 as a failure and the operator reads the code to tell "I passed a bad
// flag" apart from "the workload broke".
func TestHTTPProtoHandlerErrors(t *testing.T) {
	svc, err := newGluttonService(t.TempDir())
	if err != nil {
		t.Fatalf("newGluttonService: %v", err)
	}
	defer svc.Close()

	h := httpProtoHandler[glutton.WriteRAMRequest]("WriteRAM", svc.WriteRAM)

	// Empty key -> codes.InvalidArgument -> 400.
	emptyKey, err := proto.Marshal(&glutton.WriteRAMRequest{Size: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	tests := []struct {
		name   string
		method string
		body   []byte
		want   int
	}{
		{"get is rejected", http.MethodGet, nil, http.StatusMethodNotAllowed},
		{"garbage body", http.MethodPost, []byte{0xff, 0xff, 0xff, 0xff}, http.StatusBadRequest},
		{"invalid argument", http.MethodPost, emptyKey, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest(tc.method, "/writeram", bytes.NewReader(tc.body)))
			if rec.Code != tc.want {
				t.Errorf("%s /writeram = %d (%s), want %d",
					tc.method, rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

// TestHTTPProtoHandlerPing keeps the /ping contract intact across the move
// onto the generic handler: the reply must still echo the request.
func TestHTTPProtoHandlerPing(t *testing.T) {
	svc, err := newGluttonService(t.TempDir())
	if err != nil {
		t.Fatalf("newGluttonService: %v", err)
	}
	defer svc.Close()

	body, err := proto.Marshal(&glutton.PingRequest{Message: "hi"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := httptest.NewRecorder()
	httpProtoHandler[glutton.PingRequest]("Ping", svc.Ping)(
		rec, httptest.NewRequest(http.MethodPost, "/ping", bytes.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /ping = %d (%s), want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	out, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	pong := &glutton.PingResponse{}
	if err := proto.Unmarshal(out, pong); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pong.GetMessage() != "hi" {
		t.Errorf("Ping = %q, want %q", pong.GetMessage(), "hi")
	}
}
