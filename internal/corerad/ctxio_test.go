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

package corerad

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/mdlayher/corerad/internal/system"
	"github.com/mdlayher/ndp"
	"golang.org/x/net/ipv6"
)

func Test_receiveRetryErrors(t *testing.T) {
	t.Parallel()

	var (
		// Canned data that will be returned from Conn.ReadFrom, although we
		// don't care about the contents for the purposes of this test.
		ra = &ndp.RouterAdvertisement{}
		cm = &ipv6.ControlMessage{}
		ip = net.ParseIP("::1")

		errFatal = errors.New("fatal error")
	)

	noCancel := func() (context.Context, func()) {
		return context.WithCancel(context.Background())
	}

	readFromErr := func(err error) func() (ndp.Message, *ipv6.ControlMessage, net.IP, error) {
		return func() (ndp.Message, *ipv6.ControlMessage, net.IP, error) {
			return nil, nil, nil, err
		}
	}

	tests := []struct {
		name  string
		mkCtx func() (context.Context, func())
		conn  system.Conn
		err   error
	}{
		{
			name: "context canceled",
			mkCtx: func() (context.Context, func()) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				return ctx, cancel
			},
			err: context.Canceled,
		},
		{
			name:  "fatal error",
			mkCtx: noCancel,
			conn:  &testConn{readFrom: readFromErr(errFatal)},
			err:   errFatal,
		},
		{
			name:  "backoff success",
			mkCtx: noCancel,
			conn: &testConn{
				readFrom: func() func() (ndp.Message, *ipv6.ControlMessage, net.IP, error) {
					// The first call to ReadFrom will always fail with a
					// net.Error, so receiveRetry can back off and try again.
					// Subsequent calls will always succeed.
					var calls int
					return func() (ndp.Message, *ipv6.ControlMessage, net.IP, error) {
						defer func() { calls++ }()

						if calls == 0 {
							return nil, nil, nil, timeoutError{}
						}

						return ra, cm, ip, nil
					}
				}(),
			},
		},
		{
			name:  "backoff failure",
			mkCtx: noCancel,
			conn:  &testConn{readFrom: readFromErr(timeoutError{})},
			err:   errRetriesExhausted,
		},
		{
			name: "backoff context deadline exceeded",
			mkCtx: func() (context.Context, func()) {
				// Attempt to cancel the context during a timeout backoff/retry,
				// to trigger an alternate select case in receiveRetry.
				return context.WithTimeout(context.Background(), 25*time.Millisecond)
			},
			conn: &testConn{readFrom: readFromErr(timeoutError{})},
			err:  context.DeadlineExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := tt.mkCtx()
			defer cancel()

			if _, _, _, err := receiveRetry(ctx, tt.conn); !errors.Is(err, tt.err) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

var _ net.Error = timeoutError{}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

type testConn struct {
	readFrom        func() (ndp.Message, *ipv6.ControlMessage, net.IP, error)
	setReadDeadline func(t time.Time) error
	writeTo         func(m ndp.Message, cm *ipv6.ControlMessage, dst net.IP) error
}

func (c *testConn) ReadFrom() (ndp.Message, *ipv6.ControlMessage, net.IP, error) { return c.readFrom() }
func (c *testConn) SetReadDeadline(t time.Time) error                            { return c.setReadDeadline(t) }
func (c *testConn) WriteTo(m ndp.Message, cm *ipv6.ControlMessage, dst net.IP) error {
	return c.writeTo(m, cm, dst)
}