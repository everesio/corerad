// Copyright 2020 Matt Layher
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package crhttp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/mdlayher/corerad/internal/config"
	"github.com/mdlayher/corerad/internal/system"
	"github.com/prometheus/client_golang/prometheus"
)

// TODO: export from the package.
type tempBody struct {
	Interfaces []struct {
		Interface string `json:"interface"`
	} `json:"interfaces"`
}

func TestHandlerRoutes(t *testing.T) {
	tests := []struct {
		name              string
		state             system.State
		ifaces            []config.Interface
		prometheus, pprof bool
		path              string
		status            int
		check             func(t *testing.T, body []byte)
	}{
		{
			name:   "index",
			path:   "/",
			status: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				if !bytes.HasPrefix(body, []byte("CoreRAD")) {
					t.Fatal("CoreRAD banner was not found")
				}
			},
		},
		{
			name:   "not found",
			path:   "/foo",
			status: http.StatusNotFound,
		},
		{
			name:   "prometheus disabled",
			path:   "/metrics",
			status: http.StatusNotFound,
		},
		{
			name:       "prometheus enabled",
			prometheus: true,
			path:       "/metrics",
			status:     http.StatusOK,
			check: func(t *testing.T, body []byte) {
				if !bytes.HasPrefix(body, []byte("# HELP go_")) {
					t.Fatal("Prometheus Go collector metric was not found")
				}
			},
		},
		{
			name:   "pprof disabled",
			path:   "/debug/pprof/",
			status: http.StatusNotFound,
		},
		{
			name:   "pprof enabled",
			pprof:  true,
			path:   "/debug/pprof/goroutine?debug=1",
			status: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				if !bytes.HasPrefix(body, []byte("goroutine profile:")) {
					t.Fatal("goroutine profile was not found")
				}
			},
		},
		{
			name: "no interfaces",
			state: system.TestState{
				Forwarding: true,
			},
			path:   "/api/interfaces",
			status: http.StatusOK,
			check: func(t *testing.T, b []byte) {
				body := parseJSONBody(b)

				if diff := cmp.Diff(0, len(body.Interfaces)); diff != "" {
					t.Fatalf("unexpected number of interfaces in HTTP body (-want +got):\n%s", diff)
				}
			},
		},
		{
			name: "interfaces",
			state: system.TestState{
				Forwarding: true,
			},
			ifaces: []config.Interface{
				// One interface in each advertising and non-advertising state.
				{
					Name:            "eth0",
					Advertise:       true,
					HopLimit:        64,
					DefaultLifetime: 30 * time.Minute,
					ReachableTime:   12345 * time.Millisecond,
				},
				{Name: "eth1", Advertise: false},
			},
			path:   "/api/interfaces",
			status: http.StatusOK,
			check: func(t *testing.T, b []byte) {
				want := raBody{
					Interfaces: []interfaceBody{
						{
							Interface:   "eth0",
							Advertising: true,
							Advertisement: &routerAdvertisement{
								CurrentHopLimit:           64,
								RouterSelectionPreference: "medium",
								RouterLifetimeSeconds:     60 * 30,
								ReachableTimeMilliseconds: 12345,
							},
						},
						{
							Interface:   "eth1",
							Advertising: false,
						},
					},
				}

				if diff := cmp.Diff(want, parseJSONBody(b)); diff != "" {
					t.Fatalf("unexpected raBody (-want +got):\n%s", diff)
				}
			},
		},
		{
			name: "error fetching forwarding",
			state: system.TestState{
				Forwarding: true,
				Error:      os.ErrPermission,
			},
			ifaces: []config.Interface{
				{Name: "eth0", Advertise: true},
			},
			path:   "/api/interfaces",
			status: http.StatusInternalServerError,
			check: func(t *testing.T, b []byte) {
				if !bytes.HasPrefix(b, []byte(`failed to check interface "eth0" forwarding`)) {
					t.Fatalf("unexpected body output: %s", string(b))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a Prometheus registry with a known-good set of metrics
			// that we can check for when the Prometheus functionality is
			// enabled.
			reg := prometheus.NewPedanticRegistry()
			reg.MustRegister(prometheus.NewGoCollector())

			srv := httptest.NewServer(
				NewHandler(
					log.New(ioutil.Discard, "", 0),
					tt.state,
					tt.ifaces,
					tt.prometheus,
					tt.pprof,
					reg,
				),
			)
			defer srv.Close()

			// Use string contenation rather than path.Join because we want to
			// not escape any raw query parameters provided as part of the path.
			u, err := url.Parse(srv.URL + tt.path)
			if err != nil {
				t.Fatalf("failed to parse URL: %v", err)
			}

			c := &http.Client{Timeout: 2 * time.Second}
			res, err := c.Get(u.String())
			if err != nil {
				t.Fatalf("failed to HTTP GET: %v", err)
			}
			defer res.Body.Close()

			if diff := cmp.Diff(tt.status, res.StatusCode); diff != "" {
				t.Fatalf("unexpected HTTP status code (-want +got):\n%s", diff)
			}

			// If set, apply an additional sanity check on the response body.
			if tt.check == nil {
				return
			}

			// Don't consume a stream larger than a sane upper bound.
			const mb = 1 << 20
			body, err := ioutil.ReadAll(io.LimitReader(res.Body, 2*mb))
			if err != nil {
				t.Fatalf("failed to read HTTP body: %v", err)
			}

			tt.check(t, body)
		})
	}
}

func parseJSONBody(b []byte) raBody {
	var body raBody
	if err := json.Unmarshal(b, &body); err != nil {
		panicf("failed to unmarshal JSON: %v", err)
	}

	return body
}

func panicf(format string, a ...interface{}) {
	panic(fmt.Sprintf(format, a...))
}
