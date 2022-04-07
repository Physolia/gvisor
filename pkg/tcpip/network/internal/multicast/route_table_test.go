// Copyright 2022 The gVisor Authors.
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

package multicast

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/faketime"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/testutil"
)

const (
	defaultMinTTL             = 10
	inputNICID    tcpip.NICID = 1
	outgoingNICID tcpip.NICID = 2
	defaultNICID  tcpip.NICID = 3
)

var (
	defaultAddress            = testutil.MustParse4("192.168.1.1")
	defaultRouteKey           = RouteKey{UnicastSource: defaultAddress, MulticastDestination: defaultAddress}
	defaultOutgoingInterfaces = []OutgoingInterface{{ID: outgoingNICID, MinTTL: defaultMinTTL}}
	defaultPkt                = newPacketBuffer("hello")
)

func newPacketBuffer(body string) *stack.PacketBuffer {
	return stack.NewPacketBuffer(stack.PacketBufferOptions{
		Data: buffer.View(body).ToVectorisedView(),
	})
}

type configOption func(*Config)

func withMaxPendingQueueSize(size uint8) configOption {
	return func(c *Config) {
		c.MaxPendingQueueSize = size
	}
}

func withClock(clock tcpip.Clock) configOption {
	return func(c *Config) {
		c.Clock = clock
	}
}

func withCleanupInterval(interval time.Duration) configOption {
	return func(c *Config) {
		c.CleanupInterval = interval
	}
}

func defaultConfig(opts ...configOption) Config {
	c := &Config{
		MaxPendingQueueSize: DefaultMaxPendingQueueSize,
		PendingPacketTTL:    DefaultPendingPacketTTL,
		CleanupInterval:     DefaultCleanupInterval,
		Clock:               faketime.NewManualClock(),
	}

	for _, opt := range opts {
		opt(c)
	}

	return *c
}

func TestNewRouteTable(t *testing.T) {
	config := defaultConfig()
	if _, err := NewRouteTable(config); err != nil {
		t.Errorf("NewRouteTable(%#v) failed unexpectedly; err=%v", config, err)
	}
}

func TestNewRouteTableWithMissingClock(t *testing.T) {
	config := defaultConfig(withClock(nil))

	if _, err := NewRouteTable(config); err == nil {
		t.Errorf("NewRouteTable(%#v) succeeded for malformed input, want error", config)
	}
}

func TestNewInstalledRoute(t *testing.T) {
	clock := faketime.NewManualClock()
	clock.Advance(5 * time.Second)

	config := defaultConfig(withClock(clock))
	table, err := NewRouteTable(config)
	if err != nil {
		t.Fatalf("NewRouteTable(%#v) failed unexpectedly; err=%v", config, err)
	}

	route := table.NewInstalledRoute(inputNICID, defaultOutgoingInterfaces)
	expectedRoute := &InstalledRoute{expectedInputInterface: inputNICID, outgoingInterfaces: defaultOutgoingInterfaces, lastUsedTimestamp: clock.Now().UnixMicro()}

	if diff := cmp.Diff(expectedRoute, route, cmp.AllowUnexported(InstalledRoute{})); diff != "" {
		t.Errorf("unexpected InstalledRoute, (diff -want, +got):\n%s", diff)
	}
}

func TestPendingRouteStates(t *testing.T) {
	config := defaultConfig(withMaxPendingQueueSize(2))
	table, err := NewRouteTable(config)
	if err != nil {
		t.Fatalf("NewRouteTable(%#v) failed unexpectedly; err=%v", config, err)
	}

	pkt := newPacketBuffer("hello")
	// Queue two pending packets for the same route. The PendingRouteState should
	// transition from PendingRouteStateInstalled to PendingRouteStateAppended.
	for _, wantPendingRouteState := range [...]PendingRouteState{PendingRouteStateInstalled, PendingRouteStateAppended} {
		routeResult, err := table.GetRouteOrInsertPending(defaultRouteKey, pkt)

		if err != nil {
			t.Errorf("table.GetRouteOrInsertPending(%#v, %#v) failed unexpectedly; err=%v", defaultRouteKey, pkt, err)
		}

		if routeResult.PendingRouteState != wantPendingRouteState {
			t.Errorf("got routeResult.PendingRouteState = %s, want = %s", routeResult.PendingRouteState, wantPendingRouteState)
		}
	}

	// Queuing a third packet should yield an error since the pending queue is
	// already at max capacity.
	if _, err := table.GetRouteOrInsertPending(defaultRouteKey, pkt); err != ErrNoBufferSpace {
		t.Errorf("got table.GetRouteOrInsertPending(%#v, %#v) = (_, %v), want = (_, ErrNoBufferSpace)", defaultRouteKey, pkt, err)
	}
}

func TestPendingPacketTTL(t *testing.T) {
	// time = 0s
	clock := faketime.NewManualClock()

	// The cleanup routine will execute every 10 seconds.
	config := defaultConfig(withClock(clock), withCleanupInterval(10*time.Second))
	table, err := NewRouteTable(config)
	if err != nil {
		t.Fatalf("NewRouteTable(%#v) failed unexpectedly; err=%v", config, err)
	}

	// time = 5s
	clock.Advance(5 * time.Second)

	// firstPkt expiration = 15s
	firstPkt := newPacketBuffer("foo")
	_, err = table.GetRouteOrInsertPending(defaultRouteKey, firstPkt)

	if err != nil {
		t.Fatalf("table.GetRouteOrInsertPending(%#v, %#v) failed unexpectedly; err=%v", defaultRouteKey, firstPkt, err)
	}

	// time = 7s
	clock.Advance(2 * time.Second)

	// secondPkt expiration = 17s
	secondPkt := newPacketBuffer("foo")
	_, err = table.GetRouteOrInsertPending(defaultRouteKey, secondPkt)

	if err != nil {
		t.Fatalf("table.GetRouteOrInsertPending(%#v, %#v) failed unexpectedly; err=%v", defaultRouteKey, secondPkt, err)
	}

	// time = 12s, the cleanup routine should execute (at 10s) and not remove any
	// packets.
	clock.Advance(5 * time.Second)

	table.pendingMu.RLock()
	pendingRoute, ok := table.pendingRoutes[defaultRouteKey]
	table.pendingMu.RUnlock()

	if !ok {
		t.Fatalf("table.pendingRoutes[%#v] unexpectedly did not contain a PendingRoute", defaultRouteKey)
	}

	if len(pendingRoute.packets) != 2 {
		t.Fatalf("got len(pendingRoute.packets) = %d, want = 2", len(pendingRoute.packets))
	}

	// thirdPkt expiration = 22s
	thirdPkt := newPacketBuffer("bar")
	_, err = table.GetRouteOrInsertPending(defaultRouteKey, thirdPkt)

	if err != nil {
		t.Fatalf("table.GetRouteOrInsertPending(%#v, %#v) failed unexpectedly; err=%v", defaultRouteKey, thirdPkt, err)
	}

	// time = 21s, the cleanup routine should execute (at 20s) and remove the
	// firstPkt and the secondPkt.
	clock.Advance(9 * time.Second)
	table.pendingMu.RLock()
	pendingRoute, ok = table.pendingRoutes[defaultRouteKey]
	table.pendingMu.RUnlock()

	if !ok {
		t.Fatalf("table.pendingRoutes[%#v] unexpectedly did not contain a PendingRoute", defaultRouteKey)
	}

	if len(pendingRoute.packets) != 1 {
		t.Fatalf("got len(pendingRoute.packets) = %d, want = 1", len(pendingRoute.packets))
	}

	if !cmp.Equal(thirdPkt.Views(), pendingRoute.packets[0].pkt.Views()) {
		t.Errorf("got pendingRoute.packets[0].pkt = %v, want = %v", pendingRoute.packets[0].pkt.Views(), thirdPkt.Views())
	}

	// time = 30s, the cleanup routine should execute (at 30s) and remove the
	// entire pending route since all packets are expired.
	clock.Advance(9 * time.Second)
	table.pendingMu.RLock()
	if pendingRoute, ok := table.pendingRoutes[defaultRouteKey]; ok {
		t.Errorf("table.pendingRoutes[%#v] unexpectedly returned a PendingRoute, pendingRoute=%#v", defaultRouteKey, pendingRoute)
	}
	table.pendingMu.RUnlock()
}

func TestAddInstalledRouteWithPending(t *testing.T) {
	clock := faketime.NewManualClock()

	// Disable the cleanup routine.
	config := defaultConfig(withClock(clock), withCleanupInterval(0))
	table, err := NewRouteTable(config)
	if err != nil {
		t.Fatalf("NewRouteTable(%#v) failed unexpectedly; err=%v", config, err)
	}

	// expiredPkt expiration = 10s
	expiredPkt := newPacketBuffer("hello")
	_, err = table.GetRouteOrInsertPending(defaultRouteKey, expiredPkt)

	if err != nil {
		t.Fatalf("table.GetRouteOrInsertPending(%#v, %#v) failed unexpectedly; err=%v", defaultRouteKey, expiredPkt, err)
	}

	// time = 5s
	clock.Advance(5 * time.Second)

	// wantPkt expiration = 15s
	wantPkt := newPacketBuffer("world")
	// Queue a pending packet. This packet should later be returned in a
	// PendingRoute when table.AddInstalledRoute is invoked.
	_, err = table.GetRouteOrInsertPending(defaultRouteKey, wantPkt)

	if err != nil {
		t.Fatalf("table.GetRouteOrInsertPending(%#v, %#v) failed unexpectedly; err=%v", defaultRouteKey, wantPkt, err)
	}

	// time = 12s
	clock.Advance(7 * time.Second)
	route := table.NewInstalledRoute(inputNICID, defaultOutgoingInterfaces)

	// The expiredPkt should be dropped when a route is installed.
	pendingRoute, ok := table.AddInstalledRoute(defaultRouteKey, route)
	if !ok {
		t.Errorf("table.AddInstalledRoute(%#v, %#v) unexpectedly returned non-ok status", defaultRouteKey, route)
	}

	// Verify that the PendingRoute was removed from the pending routes table.
	table.pendingMu.RLock()
	if unexpectedRoute, ok := table.pendingRoutes[defaultRouteKey]; ok {
		t.Errorf("got table.pendingRoutes[%#v] = %#v, want = nil", defaultRouteKey, unexpectedRoute)
	}
	table.pendingMu.RUnlock()

	// Verify that packets are properly dequeued from the PendingRoute.
	pkt := pendingRoute.Dequeue()
	if !cmp.Equal(wantPkt.Views(), pkt.Views()) {
		t.Errorf("got pendingRoute.Dequeue() = %v, want = %v", pkt.Views(), wantPkt.Views())
	}

	if !pendingRoute.IsEmpty() {
		t.Errorf("got pendingRoute.IsEmpty() = false, want = true")
	}
}

func TestAddInstalledRouteWithOnlyExpiredPackets(t *testing.T) {
	clock := faketime.NewManualClock()

	// Disable the cleanup routine.
	config := defaultConfig(withClock(clock), withCleanupInterval(0))
	table, err := NewRouteTable(config)
	if err != nil {
		t.Fatalf("NewRouteTable(%#v) failed unexpectedly; err=%v", config, err)
	}

	// expiredPkt expiration = 10s
	expiredPkt := newPacketBuffer("hello")
	_, err = table.GetRouteOrInsertPending(defaultRouteKey, expiredPkt)

	if err != nil {
		t.Fatalf("table.GetRouteOrInsertPending(%#v, %#v) failed unexpectedly; err=%v", defaultRouteKey, expiredPkt, err)
	}

	// time = 11s, no pending route should be returned since the only packet in
	// the route is expired.
	clock.Advance(11 * time.Second)

	route := table.NewInstalledRoute(inputNICID, defaultOutgoingInterfaces)
	if pendingRoute, ok := table.AddInstalledRoute(defaultRouteKey, route); ok {
		t.Errorf("table.AddInstalledRoute(%#v, %#v) unexpectedly returned a PendingRoute, pendingRoute=%#v", defaultRouteKey, route, pendingRoute)
	}
}

func TestAddInstalledRouteWithNoPending(t *testing.T) {
	config := defaultConfig()
	table, err := NewRouteTable(config)
	if err != nil {
		t.Fatalf("NewRouteTable(%#v) failed unexpectedly; err=%v", config, err)
	}

	firstRoute := table.NewInstalledRoute(inputNICID, defaultOutgoingInterfaces)
	secondRoute := table.NewInstalledRoute(defaultNICID, defaultOutgoingInterfaces)

	pkt := newPacketBuffer("hello")
	for _, route := range [...]*InstalledRoute{firstRoute, secondRoute} {
		if pendingRoute, ok := table.AddInstalledRoute(defaultRouteKey, route); ok {
			t.Errorf("table.AddInstalledRoute(%#v, %#v) unexpectedly returned a PendingRoute, pendingRoute=%#v", defaultRouteKey, route, pendingRoute)
		}

		// AddInstalledRoute is invoked for the same routeKey two times. Verify
		// that the fetched InstalledRoute reflects the most recent invocation of
		// AddInstalledRoute.
		routeResult, err := table.GetRouteOrInsertPending(defaultRouteKey, pkt)

		if err != nil {
			t.Errorf("table.GetRouteOrInsertPending(%#v, %#v) failed unexpectedly; err=%v", defaultRouteKey, pkt, err)
		}

		if routeResult.PendingRouteState != PendingRouteStateNone {
			t.Errorf("got routeResult.PendingRouteState = %s, want = PendingRouteStateNone", routeResult.PendingRouteState)
		}

		if diff := cmp.Diff(route, routeResult.InstalledRoute, cmp.AllowUnexported(InstalledRoute{})); diff != "" {
			t.Errorf("unexpected route.InstalledRoute, (diff -want, +got):\n%s", diff)
		}
	}
}

func TestRemoveInstalledRoute(t *testing.T) {
	config := defaultConfig()
	table, err := NewRouteTable(config)
	if err != nil {
		t.Fatalf("NewRouteTable(%#v) failed unexpectedly; err=%v", config, err)
	}

	route := table.NewInstalledRoute(inputNICID, defaultOutgoingInterfaces)

	table.AddInstalledRoute(defaultRouteKey, route)

	if err = table.RemoveInstalledRoute(defaultRouteKey); err != nil {
		t.Errorf("table.RemoveInstalledRoute(%#v) failed unexpectedly; err=%v", defaultRouteKey, err)
	}
}

func TestRemoveInstalledRouteWithNoMatchingRoute(t *testing.T) {
	config := defaultConfig()
	table, err := NewRouteTable(config)
	if err != nil {
		t.Fatalf("NewRouteTable(%#v) failed unexpectedly; err=%v", config, err)
	}

	if err := table.RemoveInstalledRoute(defaultRouteKey); err != ErrRouteNotFound {
		t.Errorf("got table.RemoveInstalledRoute(%#v) = %v, want = ErrRouteNotFound", defaultRouteKey, err)
	}
}

func TestGetLastUsedTimestampWithNoMatchingRoute(t *testing.T) {
	config := defaultConfig()
	table, err := NewRouteTable(config)
	if err != nil {
		t.Fatalf("NewRouteTable(%#v) failed unexpectedly; err=%v", config, err)
	}

	if _, err := table.GetLastUsedTimestamp(defaultRouteKey); err != ErrRouteNotFound {
		t.Errorf("got table.GetLastUsedTimetsamp(%#v) = (_, %v), want = (_, ErrRouteNotFound)", defaultRouteKey, err)
	}
}

func TestSetLastUsedTimestamp(t *testing.T) {
	config := defaultConfig()
	table, err := NewRouteTable(config)
	if err != nil {
		t.Fatalf("NewRouteTable(%#v) failed unexpectedly; err=%v", config, err)
	}

	route := table.NewInstalledRoute(inputNICID, defaultOutgoingInterfaces)

	table.AddInstalledRoute(defaultRouteKey, route)

	expectedTime := time.UnixMilli(3000)
	route.SetLastUsedTimestamp(expectedTime)

	// Verify that the updated timestamp is actually reflected in the RouteTable.
	timestamp, err := table.GetLastUsedTimestamp(defaultRouteKey)

	if err != nil {
		t.Errorf("table.GetLastUsedTimestamp(%#v) failed unexpectedly; err=%v", defaultRouteKey, err)
	}

	if timestamp != expectedTime.UnixMicro() {
		t.Errorf("got table.GetLastUsedTimestamp(%#v) = (%d, _), want (%d, _)", defaultRouteKey, timestamp, expectedTime.UnixMicro())
	}
}
