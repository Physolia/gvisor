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

// Package multicast contains utilities for supporting multicast routing.
package multicast

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// RouteTable represents a multicast routing table.
//
// To create an instance of a RouteTable, call:
//
//	table, err := NewRouteTable(Config{..})
//
// Each entry in the RouteTable represents either an IntalledRoute or a
// PendingRoute (see the documentation of each for more details). A
// PendingRoute is automatically inserted when an InstalledRoute is not found
// in the table:
//
//	result, err := table.GetRouteOrInsertPending(key, pkt)
//
// PendingRoutes cannot be used to forward multicast packets. Consequently, a
// multicast routing service should transition the entry to an InstalledRoute.
// To create an InstalledRoute, call:
//
//	route := table.NewInstalledRoute(inputInterface, outgoingInterfaces)
//
// The InstalledRoute can then be added to the table by calling:
//
//	pendingRoute, err := table.AddInstalledRoute(key, route)
//
// The caller is responsible for flushing the packet queue in the returned
// PendingRoute, e.g.:
//
//	for !pendingRoute.IsEmpty() {
//		pkt := pendingRoute.Dequeue()
//		...
//	}
//
// Finally, when an InstalledRoute is used to forward a packet, the caller is
// responsible for updating the last used timestamp. To do so, call:
//
//	installedRoute.SetLastUsedTimestamp(time.Now())
type RouteTable struct {
	// Internally, InstalledRoutes and PendingRoutes are stored and locked
	// separately. A couple of reasons for structuring the table this way:
	//
	// 1. We can avoid write locking InstalledRoutes when pending packets are
	//		being queued. In other words, the happy path of reading InstalledRoutes
	//		doesn't require an exclusive lock.
	// 2. The cleanup process for expired routes only needs to operate on
	//		PendingRoutes. Like above, a write lock on the InstalledRoutes can be
	//		avoided.
	// 3. This structure is similar to the Linux implementation:
	//		https://github.com/torvalds/linux/blob/cffb2b72d3ed47f5093d128bd44d9ce136b6b5af/include/linux/mroute_base.h#L250

	// TODO(https://gvisor.dev/issue/7338): Implement time based expiration of
	// pending packets.

	installedMu sync.RWMutex
	// +checklocks:installedMu
	installedRoutes map[RouteKey]*InstalledRoute

	pendingMu sync.RWMutex
	// +checklocks:pendingMu
	pendingRoutes map[RouteKey]*PendingRoute

	config Config
}

var (
	// ErrNoBufferSpace indicates that no buffer space is available in the
	// PendingRoute packet queue.
	ErrNoBufferSpace = errors.New("unable to queue packet, no buffer space available")
	// ErrRouteNotFound indicates that a matching InstalledRoute was not found.
	ErrRouteNotFound = errors.New("unable to find InstalledRoute")
)

// RouteKey represents an entry key in the RouteTable.
type RouteKey struct {
	UnicastSource        tcpip.Address
	MulticastDestination tcpip.Address
}

// InstalledRoute represents a route that is in the "installed" state.
//
// If a route is in the "installed" state, then it may be used to forward
// multicast packets.
//
// An instance of this type can be obtained using the
// RouteTable.NewInstalledRoute method.
type InstalledRoute struct {
	expectedInputInterface tcpip.NICID
	outgoingInterfaces     []OutgoingInterface
	// +checkatomic
	lastUsedTimestamp int64
}

// ExpectedInputInterface returns the expected input interface for the route.
func (r *InstalledRoute) ExpectedInputInterface() tcpip.NICID {
	return r.expectedInputInterface
}

// OutgoingInterfaces returns the outgoing interfaces for the route.
func (r *InstalledRoute) OutgoingInterfaces() []OutgoingInterface {
	return r.outgoingInterfaces
}

// LastUsedTimestamp returns a Unix based timestamp in microseconds that
// corresponds to the last time the route was used or updated.
func (r *InstalledRoute) LastUsedTimestamp() int64 {
	return atomic.LoadInt64(&r.lastUsedTimestamp)
}

// SetLastUsedTimestamp sets the time that the route was last used.
//
// Callers should invoke this anytime the route is used to forward a packet.
func (r *InstalledRoute) SetLastUsedTimestamp(time time.Time) {
	atomic.StoreInt64(&r.lastUsedTimestamp, time.UnixMicro())
}

// OutgoingInterface represents an interface that packets should be forwarded
// out of.
type OutgoingInterface struct {
	// ID corresponds to the outgoing NIC.
	ID tcpip.NICID
	// MinTTL represents the minumum TTL/HopLimit a multicast packet must have to
	// be sent through the outgoing interface.
	MinTTL uint8
}

// PendingRoute represents a route that is in the "pending" state.
//
// A route is in the "pending" state if an InstalledRoute does not yet exist
// for the entry. For such routes, packets are added to an expiring queue until
// an InstalledRoute is added. When that happens, callers should flush the
// queue using the newly installed route.
type PendingRoute struct {
	packets []*stack.PacketBuffer
}

func newPendingRoute(maxSize uint8) *PendingRoute {
	return &PendingRoute{packets: make([]*stack.PacketBuffer, 0, maxSize)}
}

func (p *PendingRoute) enqueue(pkt *stack.PacketBuffer, maxSize uint8) error {
	if len(p.packets) >= int(maxSize) {
		// The incoming packet is rejected if the pending queue is already at max
		// capcity. This behavior matches the Linux implementation:
		// https://github.com/torvalds/linux/blob/ae085d7f9365de7da27ab5c0d16b12d51ea7fca9/net/ipv4/ipmr.c#L1147
		return ErrNoBufferSpace
	}
	p.packets = append(p.packets, pkt)
	return nil
}

// Dequeue removes the first element in the queue and returns is.
func (p *PendingRoute) Dequeue() *stack.PacketBuffer {
	val := p.packets[0]
	p.packets = p.packets[1:]
	return val
}

// IsEmpty returns true if the queue contains no more elements. Otherwise,
// returns false.
func (p *PendingRoute) IsEmpty() bool {
	return len(p.packets) == 0
}

// DefaultMaxPendingQueueSize corresponds to the number of elements that can be
// in the packet queue for a PendingRoute.
//
// Matches the Linux default queue size:
// https://github.com/torvalds/linux/blob/26291c54e111ff6ba87a164d85d4a4e134b7315c/net/ipv6/ip6mr.c#L1186
const DefaultMaxPendingQueueSize uint8 = 3

// Config represents the options for configuring a RouteTable.
type Config struct {
	// MaxPendingQueueSize corresponds to the maximum number of queued packets
	// for a PendingRoute.
	//
	// If the caller attempts to queue a packet and the queue already contains
	// MaxPendingQueueSize elements, then the packet will be rejected and should
	// not be forwarded.
	MaxPendingQueueSize uint8

	// Clock represents the clock that should be used to obtain the current time.
	//
	// This field is required and must have a non-nil value.
	Clock tcpip.Clock
}

// DefaultConfig returns the default configuration for the table.
func DefaultConfig(clock tcpip.Clock) Config {
	return Config{MaxPendingQueueSize: DefaultMaxPendingQueueSize, Clock: clock}
}

// NewRouteTable instatiates a RouteTable from the provided config.
//
// An error is returned if the config is not valid.
func NewRouteTable(config Config) (*RouteTable, error) {
	if err := config.isValid(); err != nil {
		return nil, err
	}
	return &RouteTable{installedRoutes: make(map[RouteKey]*InstalledRoute), pendingRoutes: make(map[RouteKey]*PendingRoute), config: config}, nil
}

func (c Config) isValid() error {
	if c.Clock == nil {
		return errors.New("clock must not be nil")
	}
	return nil
}

// NewInstalledRoute instatiates an InstalledRoute for the table.
func (r *RouteTable) NewInstalledRoute(inputInterface tcpip.NICID, outgoingInterfaces []OutgoingInterface) *InstalledRoute {
	return &InstalledRoute{expectedInputInterface: inputInterface, outgoingInterfaces: outgoingInterfaces, lastUsedTimestamp: r.config.Clock.Now().UnixMicro()}
}

// GetRouteResult represents the result of calling
// RouteTable.GetRouteOrInsertPending.
type GetRouteResult struct {
	// PendingRouteState represents the observed state of any applicable
	// PendingRoute.
	PendingRouteState

	// InstalledRoute represents the existing InstalledRoute. This field will
	// only be populated if the PendingRouteState is PendingRouteStateNone.
	*InstalledRoute
}

// PendingRouteState represents the state of a PendingRoute as observed by the
// RouteTable.GetRouteOrInsertPending method.
type PendingRouteState uint8

const (
	// PendingRouteStateNone indicates that no PendingRoute exists. In such a
	// case, the GetRouteResult will contain an InstalledRoute.
	PendingRouteStateNone PendingRouteState = iota
	// PendingRouteStateAppended indicates that the packet was queued in an
	// existing PendingRoute.
	PendingRouteStateAppended
	// PendingRouteStateInstalled indicates that a PendingRoute was newly
	// inserted into the RouteTable. In such a case, callers should typically
	// emit a missing route event.
	PendingRouteStateInstalled
)

func (e PendingRouteState) String() string {
	switch e {
	case PendingRouteStateNone:
		return "PendingRouteStateNone"
	case PendingRouteStateAppended:
		return "PendingRouteStateAppended"
	case PendingRouteStateInstalled:
		return "PendingRouteStateInstalled"
	default:
		return fmt.Sprintf("%d", uint8(e))
	}
}

// GetRouteOrInsertPending attempts to fetch the InstalledRoute that matches
// the provided key.
//
// If no matching InstalledRoute is found, then the pkt is queued in a
// PendingRoute. The GetRouteResult.PendingRouteState will indicate whether the
// pkt was queued in a new PendingRoute or an existing one.
//
// If the relevant PendingRoute queue is at max capacity, then ErrNoBufferSpace
// is returned. In such a case, callers are typically expected to only deliver
// the pkt locally (if relevant).
func (r *RouteTable) GetRouteOrInsertPending(key RouteKey, pkt *stack.PacketBuffer) (*GetRouteResult, error) {
	r.installedMu.RLock()
	defer r.installedMu.RUnlock()

	if route, ok := r.installedRoutes[key]; ok {
		return &GetRouteResult{PendingRouteState: PendingRouteStateNone, InstalledRoute: route}, nil
	}

	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()

	pendingRoute, pendingRouteState := r.getOrInsertPendingRouteRWLocked(key)
	if err := pendingRoute.enqueue(pkt, r.config.MaxPendingQueueSize); err != nil {
		return nil, err
	}

	return &GetRouteResult{PendingRouteState: pendingRouteState, InstalledRoute: nil}, nil
}

// +checklocks:r.pendingMu
func (r *RouteTable) getOrInsertPendingRouteRWLocked(key RouteKey) (*PendingRoute, PendingRouteState) {
	if pendingRoute, ok := r.pendingRoutes[key]; ok {
		return pendingRoute, PendingRouteStateAppended
	}

	pendingRoute := newPendingRoute(r.config.MaxPendingQueueSize)
	r.pendingRoutes[key] = pendingRoute
	return pendingRoute, PendingRouteStateInstalled

}

// AddInstalledRoute adds the provided route to the table.
//
// If the route was previously in the pending state, then the PendingRoute is
// returned along with true. The caller is responsible for flushing the
// returned packet queue.
//
// Conversely, if the route was not in a pending state, then any existing
// InstalledRoute will be overwritten and (nil, false) will be returned.
func (r *RouteTable) AddInstalledRoute(key RouteKey, route *InstalledRoute) (*PendingRoute, bool) {
	r.installedMu.Lock()
	defer r.installedMu.Unlock()
	r.installedRoutes[key] = route

	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()

	pendingRoute, ok := r.pendingRoutes[key]
	delete(r.pendingRoutes, key)
	return pendingRoute, ok
}

// RemoveInstalledRoute deletes any installed route that matches the provided
// key.
//
// If no matching entry is found, then ErrRouteNotFound is returned.
func (r *RouteTable) RemoveInstalledRoute(key RouteKey) error {
	r.installedMu.Lock()
	defer r.installedMu.Unlock()

	if _, ok := r.installedRoutes[key]; ok {
		delete(r.installedRoutes, key)
		return nil
	}

	return ErrRouteNotFound
}

// GetLastUsedTimestamp returns the last time the route that corresponds to key
// was used or updated.
//
// If no matching entry is found, then ErrRouteNotFound is returned.
func (r *RouteTable) GetLastUsedTimestamp(key RouteKey) (int64, error) {
	r.installedMu.RLock()
	defer r.installedMu.RUnlock()

	if route, ok := r.installedRoutes[key]; ok {
		return route.LastUsedTimestamp(), nil
	}
	return 0, ErrRouteNotFound
}
