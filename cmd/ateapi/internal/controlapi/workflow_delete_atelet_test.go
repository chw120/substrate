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
	"net"
	"sync"
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store/storetest"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// terminateRecorder is a fake atelet that records every Terminate it is sent.
type terminateRecorder struct {
	ateletpb.UnimplementedAteomHerderServer

	mu   sync.Mutex
	reqs []*ateletpb.TerminateRequest
}

func (f *terminateRecorder) Terminate(_ context.Context, req *ateletpb.TerminateRequest) (*ateletpb.TerminateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, proto.Clone(req).(*ateletpb.TerminateRequest))
	return &ateletpb.TerminateResponse{}, nil
}

func (f *terminateRecorder) received() []*ateletpb.TerminateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*ateletpb.TerminateRequest(nil), f.reqs...)
}

// newTerminateRecordingDialer builds a dialer that resolves node-1 to an
// in-process recorder, so a test can tell an atelet that was never called from
// one that was called and answered.
func newTerminateRecordingDialer(t *testing.T) (*AteletDialer, *terminateRecorder) {
	t.Helper()

	fake := &terminateRecorder{}
	srv := grpc.NewServer()
	ateletpb.RegisterAteomHerderServer(srv, fake)
	lis := bufconn.Listen(1 << 20)
	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("fake atelet server exited: %v", err)
		}
	}()
	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}))
	if err != nil {
		t.Fatalf("connecting to the fake atelet: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		srv.Stop()
	})

	ateletPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ateletNamespace, Name: "atelet-1", UID: "atelet-uid"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
	}
	dialer := NewAteletDialer(newTestAteletIndexer(t, ateletPod), "", "")
	dialer.ateletConns.Add("atelet-uid", conn)
	return dialer, fake
}

// bindWorkerForAssignment creates the Worker and the assignment row that
// wireTestAssignment refers to, so the delete path's workerHostsActor check
// finds the actor still hosted.
func bindWorkerForAssignment(t *testing.T, ctx context.Context, st store.Interface, actorRef resources.ActorRef) {
	t.Helper()
	assignment := wireTestAssignment()
	if _, err := st.CreateWorker(ctx, &ateapipb.Worker{
		Metadata:        &ateapipb.ResourceMetadata{Name: assignment.GetWorker().GetName()},
		WorkerNamespace: assignment.GetWorkerNamespace(),
		WorkerPool:      assignment.GetWorkerPool(),
		WorkerPod:       assignment.GetWorkerPod(),
		WorkerPodUid:    assignment.GetWorkerPodUid(),
		NodeName:        assignment.GetNodeName(),
		Status:          &ateapipb.WorkerStatus{},
	}); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	actor, err := st.GetActor(ctx, actorRef)
	if err != nil {
		t.Fatalf("GetActor: %v", err)
	}
	seedAssignment(t, st, assignment.GetWorker().GetName(), &ateapipb.ActorAssignment{
		Actor:    &ateapipb.ObjectRef{Atespace: actorRef.Atespace, Name: actorRef.Name},
		ActorUid: actor.GetMetadata().GetUid(),
	})
}

// TestDeleteActor_AteletTerminateGatedOnWorkerAssignment pins what decides
// whether a delete reaches the node at all. The two rows differ in one field
// of the stored Actor — status.worker_assignment — and nothing else.
func TestDeleteActor_AteletTerminateGatedOnWorkerAssignment(t *testing.T) {
	tests := []struct {
		name           string
		withAssignment bool
		wantTerminates int
	}{
		{
			name:           "no worker assignment: the node is never told",
			withAssignment: false,
			wantTerminates: 0,
		},
		{
			name:           "worker assignment present: the node is told",
			withAssignment: true,
			wantTerminates: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, cleanup := storetest.SetupTestStore(t)
			defer cleanup()

			w := newTestActorWorkflow(t, st, "ns", "tmpl1")
			dialer, fake := newTerminateRecordingDialer(t)
			w.dialer = dialer

			actorRef := resources.ActorRef{Atespace: "team-a", Name: "id1"}
			seedWorkflowActor(t, ctx, st, actorRef, "ns", "tmpl1",
				ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				func(a *ateapipb.Actor) {
					if tc.withAssignment {
						a.Status.WorkerAssignment = wireTestAssignment()
					}
				})
			if tc.withAssignment {
				bindWorkerForAssignment(t, ctx, st, actorRef)
			}

			if _, err := w.DeleteActor(ctx, actorRef, false); err != nil {
				t.Fatalf("DeleteActor: %v", err)
			}
			if got := len(fake.received()); got != tc.wantTerminates {
				t.Errorf("atelet received %d Terminate requests, want %d", got, tc.wantTerminates)
			}
		})
	}
}

// TestSuspendThenDeleteNeverReachesAtelet walks the ordinary path an actor
// takes out of the system: it is running on a worker, it is suspended, and
// then it is deleted from SUSPENDED — the only state a plain delete accepts.
// Suspending clears the worker assignment, so by the time the delete looks for
// one there is none, and the node is never told the actor is gone.
//
// It records today's behavior rather than the behavior we want: the node keeps
// the actor's state directory, and only the delete can ask it to let go. A
// change that reaches atelet here should update the assertion, not restore it.
func TestSuspendThenDeleteNeverReachesAtelet(t *testing.T) {
	ctx := context.Background()
	st, cleanup := storetest.SetupTestStore(t)
	defer cleanup()

	w := newTestActorWorkflow(t, st, "ns", "tmpl1")
	dialer, fake := newTerminateRecordingDialer(t)
	w.dialer = dialer

	actorRef := resources.ActorRef{Atespace: "team-a", Name: "id1"}
	seedWorkflowActor(t, ctx, st, actorRef, "ns", "tmpl1",
		ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
		func(a *ateapipb.Actor) { a.Status.WorkerAssignment = wireTestAssignment() })
	bindWorkerForAssignment(t, ctx, st, actorRef)

	template, err := st.GetActorTemplate(ctx, resources.ActorTemplateRef{Atespace: "ns", Name: "tmpl1"})
	if err != nil {
		t.Fatalf("GetActorTemplate: %v", err)
	}
	suspended, err := w.ensureSuspendedFinalized(ctx, actorRef, template)
	if err != nil {
		t.Fatalf("ensureSuspendedFinalized: %v", err)
	}
	if got := suspended.GetStatus().GetState(); got != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("state after suspend = %v, want SUSPENDED", got)
	}
	if got := suspended.GetStatus().GetWorkerAssignment(); got != nil {
		t.Fatalf("suspend left a worker assignment behind: %v", got)
	}

	if _, err := w.DeleteActor(ctx, actorRef, false); err != nil {
		t.Fatalf("DeleteActor: %v", err)
	}
	if got := fake.received(); len(got) != 0 {
		t.Errorf("atelet received %d Terminate requests over a suspend-then-delete, want 0", len(got))
	}
}
