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

package ch

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadAPIResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		wantErr  bool
		want     []string // substrings the error must contain
	}{
		{
			name:     "success",
			response: "HTTP/1.1 204 No Content\r\nServer: Cloud Hypervisor API\r\n\r\n",
		},
		{
			// The failure this reader exists for: CH names the reason in the body
			// and says only "Internal Server Error" on the status line.
			name: "error body is reported",
			response: "HTTP/1.1 500 Internal Server Error\r\nServer: Cloud Hypervisor API\r\n" +
				"Content-Type: application/json\r\nContent-Length: 46\r\n\r\n" +
				`{"error":"Error adding network device to VM"}` + "\n",
			wantErr: true,
			want:    []string{"vm.add-net failed", "500", "Error adding network device to VM"},
		},
		{
			name:     "no body falls back to the status line",
			response: "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n",
			wantErr:  true,
			want:     []string{"vm.add-net failed", "404 Not Found"},
		},
		{
			name:     "a body with no Content-Length is not read",
			response: "HTTP/1.1 500 Internal Server Error\r\n\r\nsomething",
			wantErr:  true,
			want:     []string{"vm.add-net failed", "500"},
		},
		{
			// A truncated body must not turn into a different, more confusing error.
			name:     "short body degrades to the status line",
			response: "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 500\r\n\r\ntoo short",
			wantErr:  true,
			want:     []string{"vm.add-net failed", "500 Internal Server Error"},
		},
		{
			name:     "unparsable Content-Length degrades to the status line",
			response: "HTTP/1.1 500 Internal Server Error\r\nContent-Length: banana\r\n\r\nbody",
			wantErr:  true,
			want:     []string{"vm.add-net failed", "500"},
		},
		{
			name:     "headers are matched case-insensitively",
			response: "HTTP/1.1 400 Bad Request\r\ncontent-length: 4\r\n\r\nnope",
			wantErr:  true,
			want:     []string{"400", "nope"},
		},
		{
			name:     "truncated response",
			response: "HTTP/1.1 200",
			wantErr:  true,
			want:     []string{"reading vm.add-net response"},
		},
		{
			name:     "a 2xx other than 200 succeeds",
			response: "HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := readAPIResponse("vm.add-net", bufio.NewReader(strings.NewReader(tc.response)))
			if tc.wantErr != (err != nil) {
				t.Fatalf("readAPIResponse() = %v, want error: %v", err, tc.wantErr)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("readAPIResponse() = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// A body longer than the cap is truncated rather than quoted whole, and the
// reader still returns instead of waiting for bytes that may never come.
func TestReadAPIResponseCapsTheBody(t *testing.T) {
	body := strings.Repeat("x", maxAPIErrorBody*2)
	response := "HTTP/1.1 500 Internal Server Error\r\nContent-Length: " +
		"8192\r\n\r\n" + body
	err := readAPIResponse("vm.restore", bufio.NewReader(strings.NewReader(response)))
	if err == nil {
		t.Fatal("readAPIResponse() = nil, want an error")
	}
	if got := len(err.Error()); got > maxAPIErrorBody+128 {
		t.Errorf("readAPIResponse() returned a %d-byte error, want it capped near %d", got, maxAPIErrorBody)
	}
}
