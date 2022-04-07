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
type RouteTable struct {
	// Internally, installed and pending routes are stored and locked separately
	// A couple of reasons for structuring the table this way:
	//
	// 1. We can avoid write locking installed routes when pending packets are
	//		being queued. In other words, the happy path of reading installed
	//		routes doesn't require an exclusive lock.
	// 2. The cleanup process for expired routes only needs to operate on pending
	//		routes. Like above, a write lock on the installed routes can be
	//		avoided.
	// 3. This structure is similar to the Linux implementation:
	//		https://github.com/torvalds/linux/blob/cffb2b72d3ed47f5093d128bd44d9ce136b6b5af/include/linux/mroute_base.h#L250

	// TODO(https://gvisor.dev/issue/7338): Implement time based expiration of
	// pending packets.

	// The installedMu lock should typically be acquired before the pendingMu
	// lock. This ensures that installed routes can continue to be read even when
	// the pending routes are write locked.

	installedMu sync.RWMutex
	// +checklocks:installedMu
	installedRoutes map[RouteKey]*InstalledRoute

	pendingMu sync.RWMutex
	// +checklocks:pendingMu
	pendingRoutes map[RouteKey]*PendingRoute

	config Config

	// cleanupPendingRoutesTimer is a timer that triggers a routine to remove
	// pending packets/routes that are expired.
	cleanupPendingRoutesTimer tcpip.Timer
}

var (
	// ErrNoBufferSpace indicates that no buffer space is available in the
	// pending route packet queue.
	ErrNoBufferSpace = errors.New("unable to queue packet, no buffer space available")
	// ErrRouteNotFound indicates that a matching installed route was not found.
	ErrRouteNotFound = errors.New("unable to find installed route")
)

// RouteKey represents an entry key in the RouteTable.
type RouteKey struct {
	UnicastSource        tcpip.Address
	MulticastDestination tcpip.Address
}

// InstalledRoute represents a route that is in the installed state.
//
// If a route is in the installed state, then it may be used to forward
// multicast packets.
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
// A route is in the pending state if an installed route does not yet exist
// for the entry. For such routes, packets are added to an expiring queue until
// a route is installed.
type PendingRoute struct {
	packets []*pendingPacket
}

type pendingPacket struct {
	pkt *stack.PacketBuffer

	// expiration is the timestamp at which the packet should be expired.
	//
	// If this value is before the current time, then this pending packet will
	// be dropped.
	expiration tcpip.MonotonicTime
}

func newPendingRoute(maxSize uint8) *PendingRoute {
	return &PendingRoute{packets: make([]*pendingPacket, 0, maxSize)}
}

func (p *PendingRoute) removeExpired(currentTime tcpip.MonotonicTime) {
	for pkt, ok := p.peek(); ok && currentTime.After(pkt.expiration); pkt, ok = p.peek() {
		p.Dequeue()
	}
}

func (p *PendingRoute) enqueue(pkt *pendingPacket, maxSize uint8) error {
	if len(p.packets) >= int(maxSize) {
		// The incoming packet is rejected if the pending queue is already at max
		// capcity. This behavior matches the Linux implementation:
		// https://github.com/torvalds/linux/blob/ae085d7f9365de7da27ab5c0d16b12d51ea7fca9/net/ipv4/ipmr.c#L1147
		return ErrNoBufferSpace
	}
	p.packets = append(p.packets, pkt)
	return nil
}

func (p *PendingRoute) peek() (*pendingPacket, bool) {
	if p.IsEmpty() {
		return nil, false
	}
	return p.packets[0], true
}

// Dequeue removes the first element in the queue and returns is.
func (p *PendingRoute) Dequeue() *stack.PacketBuffer {
	val := p.packets[0]
	p.packets = p.packets[1:]
	return val.pkt
}

// IsEmpty returns true if the queue contains no more elements. Otherwise,
// returns false.
func (p *PendingRoute) IsEmpty() bool {
	return len(p.packets) == 0
}

const (
	// DefaultMaxPendingQueueSize corresponds to the number of elements that can be
	// in the packet queue for a pending route.
	//
	// Matches the Linux default queue size:
	// https://github.com/torvalds/linux/blob/26291c54e111ff6ba87a164d85d4a4e134b7315c/net/ipv6/ip6mr.c#L1186
	DefaultMaxPendingQueueSize uint8 = 3
	// DefaultPendingPacketTTL is the default maximum lifetime of a queued
	// pending packet.
	DefaultPendingPacketTTL time.Duration = 10 * time.Second
	// DefaultCleanupInterval is the default frequency of the routine that
	// expires pending packets/routes.
	DefaultCleanupInterval time.Duration = 10 * time.Second
)

// Config represents the options for configuring a RouteTable.
type Config struct {
	// MaxPendingQueueSize corresponds to the maximum number of queued packets
	// for a pending route.
	//
	// If the caller attempts to queue a packet and the queue already contains
	// MaxPendingQueueSize elements, then the packet will be rejected and should
	// not be forwarded.
	MaxPendingQueueSize uint8

	// PendingPacketTTL is the maximum lifetime of a queued packet.
	//
	// Queued packets that exceed this lifetime will be dropped.
	PendingPacketTTL time.Duration

	// CleanupInterval corresponds to how often the routine to expire pending
	// packets/routes is run.
	//
	// If this value is less than or equal to 0, then the cleanup routine is
	// never executed.
	CleanupInterval time.Duration

	// Clock represents the clock that should be used to obtain the current time.
	//
	// This field is required and must have a non-nil value.
	Clock tcpip.Clock
}

// DefaultConfig returns the default configuration for the table.
func DefaultConfig(clock tcpip.Clock) Config {
	return Config{
		MaxPendingQueueSize: DefaultMaxPendingQueueSize,
		PendingPacketTTL:    DefaultPendingPacketTTL,
		CleanupInterval:     DefaultCleanupInterval,
		Clock:               clock,
	}
}

// NewRouteTable instatiates a RouteTable from the provided config.
//
// An error is returned if the config is not valid.
func NewRouteTable(config Config) (*RouteTable, error) {
	if err := config.isValid(); err != nil {
		return nil, err
	}

	table := &RouteTable{
		installedRoutes: make(map[RouteKey]*InstalledRoute),
		pendingRoutes:   make(map[RouteKey]*PendingRoute),
		config:          config,
	}

	if table.config.CleanupInterval > 0 {
		table.cleanupPendingRoutesTimer = table.config.Clock.AfterFunc(table.config.CleanupInterval, table.cleanupPendingRoutes)
	}

	return table, nil
}

func (c Config) isValid() error {
	if c.Clock == nil {
		return errors.New("clock must not be nil")
	}
	return nil
}

func (r *RouteTable) cleanupPendingRoutes() {
	currentTime := r.config.Clock.NowMonotonic()

	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()

	for key, route := range r.pendingRoutes {
		route.removeExpired(currentTime)
		if route.IsEmpty() {
			// All packets are expired. Remove the entire pending route.
			delete(r.pendingRoutes, key)
		}
	}

	r.cleanupPendingRoutesTimer.Reset(r.config.CleanupInterval)
}

func (r *RouteTable) newPendingPacket(pkt *stack.PacketBuffer) *pendingPacket {
	return &pendingPacket{
		pkt:        pkt,
		expiration: r.config.Clock.NowMonotonic().Add(r.config.PendingPacketTTL),
	}
}

// NewInstalledRoute instatiates an installed route for the table.
func (r *RouteTable) NewInstalledRoute(inputInterface tcpip.NICID, outgoingInterfaces []OutgoingInterface) *InstalledRoute {
	return &InstalledRoute{
		expectedInputInterface: inputInterface,
		outgoingInterfaces:     outgoingInterfaces,
		lastUsedTimestamp:      r.config.Clock.Now().UnixMicro(),
	}
}

// GetRouteResult represents the result of calling
// RouteTable.GetRouteOrInsertPending.
type GetRouteResult struct {
	// PendingRouteState represents the observed state of any applicable
	// PendingRoute.
	PendingRouteState

	// InstalledRoute represents the existing installed route. This field will
	// only be populated if the PendingRouteState is PendingRouteStateNone.
	*InstalledRoute
}

// PendingRouteState represents the state of a PendingRoute as observed by the
// RouteTable.GetRouteOrInsertPending method.
type PendingRouteState uint8

const (
	// PendingRouteStateNone indicates that no pending route exists. In such a
	// case, the GetRouteResult will contain an InstalledRoute.
	PendingRouteStateNone PendingRouteState = iota
	// PendingRouteStateAppended indicates that the packet was queued in an
	// existing pending route.
	PendingRouteStateAppended
	// PendingRouteStateInstalled indicates that a pending route was newly
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

// GetRouteOrInsertPending attempts to fetch the installed route that matches
// the provided key.
//
// If no matching installed route is found, then the pkt is queued in a
// pending route. The GetRouteResult.PendingRouteState will indicate whether
// the pkt was queued in a new pending route or an existing one.
//
// If the relevant pending route queue is at max capacity, then
// ErrNoBufferSpace is returned. In such a case, callers are typically expected
// to only deliver the pkt locally (if relevant).
func (r *RouteTable) GetRouteOrInsertPending(key RouteKey, pkt *stack.PacketBuffer) (*GetRouteResult, error) {
	r.installedMu.RLock()
	defer r.installedMu.RUnlock()

	if route, ok := r.installedRoutes[key]; ok {
		return &GetRouteResult{PendingRouteState: PendingRouteStateNone, InstalledRoute: route}, nil
	}

	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()

	pendingRoute, pendingRouteState := r.getOrInsertPendingRouteRWLocked(key)
	if err := pendingRoute.enqueue(r.newPendingPacket(pkt), r.config.MaxPendingQueueSize); err != nil {
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
// If the route was previously in the pending state, then the pending route is
// returned along with true. The caller is responsible for flushing the
// returned packet queue.
//
// Conversely, if the route was not in a pending state, then any existing
// installed route will be overwritten and (nil, false) will be returned.
func (r *RouteTable) AddInstalledRoute(key RouteKey, route *InstalledRoute) (*PendingRoute, bool) {
	r.installedMu.Lock()
	defer r.installedMu.Unlock()
	r.installedRoutes[key] = route

	r.pendingMu.Lock()
	pendingRoute, ok := r.pendingRoutes[key]
	delete(r.pendingRoutes, key)
	r.pendingMu.Unlock()

	if !ok {
		return nil, false
	}

	// The pending route may contain expired packets since the cleanup routine is
	// only run periodically. Such packets should be filtered out before
	// returning.
	pendingRoute.removeExpired(r.config.Clock.NowMonotonic())

	if pendingRoute.IsEmpty() {
		// All packets were expired. Nothing to return.
		return nil, false
	}
	return pendingRoute, true
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
