// Copyright 2019 Matt Layher
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
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"time"

	"github.com/mdlayher/corerad/internal/config"
	"github.com/mdlayher/ndp"
	"golang.org/x/net/ipv6"
)

// An Advertiser sends NDP router advertisements.
type Advertiser struct {
	c   *ndp.Conn
	ifi *net.Interface
	ip  net.IP

	cfg config.Interface
	b   *builder

	ll *log.Logger
}

// NewAdvertiser creates an Advertiser for the specified interface. If ll is
// nil, logs are discarded.
func NewAdvertiser(cfg config.Interface, ll *log.Logger) (*Advertiser, error) {
	if ll == nil {
		ll = log.New(ioutil.Discard, "", 0)
	}

	ifi, err := net.InterfaceByName(cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to look up interface %q: %v", cfg.Name, err)
	}

	c, ip, err := ndp.Dial(ifi, ndp.LinkLocal)
	if err != nil {
		// Explicitly wrap this error for caller.
		return nil, fmt.Errorf("failed to create NDP listener: %w", err)
	}

	// We only want to accept router solicitation messages.
	var f ipv6.ICMPFilter
	f.SetAll(true)
	f.Accept(ipv6.ICMPTypeRouterSolicitation)

	if err := c.SetICMPFilter(&f); err != nil {
		return nil, fmt.Errorf("failed to apply ICMPv6 filter: %v", err)
	}

	// We are now a router.
	if err := c.JoinGroup(net.IPv6linklocalallrouters); err != nil {
		return nil, fmt.Errorf("failed to join IPv6 link-local all routers multicast group: %v", err)
	}

	return &Advertiser{
		c:   c,
		ifi: ifi,
		ip:  ip,

		cfg: cfg,
		// Set up a builder to construct RAs from configuration.
		b: &builder{
			// Fetch the configured interface's addresses.
			Addrs: ifi.Addrs,
		},

		ll: ll,
	}, nil
}

// Close closes the Advertiser's connection.
func (a *Advertiser) Close() error {
	return a.c.Close()
}

// Advertise begins sending router advertisements at regular intervals. Advertise
// will block until ctx is canceled or an error occurs.
func (a *Advertiser) Advertise(ctx context.Context) error {
	a.logf("initialized, sending router advertisements from %s", a.ip)

	for {
		// Enable cancelation before sending any messages, if necessary.
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// Build a router advertisement from configuration and always append
		// the source address option.
		ra, err := a.b.Build(a.cfg)
		if err != nil {
			return fmt.Errorf("failed to build NDP router advertisement: %v", err)
		}

		// TODO: apparently it is also valid to omit this, but we can think
		// about that later.
		ra.Options = append(ra.Options, &ndp.LinkLayerAddress{
			Direction: ndp.Source,
			Addr:      a.ifi.HardwareAddr,
		})

		if err := a.c.WriteTo(ra, nil, net.IPv6linklocalallnodes); err != nil {
			return fmt.Errorf("failed to send NDP router advertisement: %v", err)
		}

		// TODO: set via configuration.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

func (a *Advertiser) logf(format string, v ...interface{}) {
	a.ll.Println(a.ifi.Name + ": " + fmt.Sprintf(format, v...))
}