// Copyright 2022 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at //
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stack

import (
	"fmt"
	"math/bits"
	"runtime"
	"sync"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/refsvfs2"
)

// enableLogging indicates whether reference-related events should be logged (with
// stack traces). This is false by default and should only be set to true for
// debugging purposes, as it can generate an extremely large amount of output
// and drastically degrade performance.
const enableLogging = false

const upgradingState = 1 << 62

const upgradedState = 1 << 63

// Refs keeps a reference count. It calls a destructor when it reaches zero. It
// prevents double-decrementing by giving out unique references called Tickets
// that are only valid for a single call to DecRef().
//
// Refs is optimized for small-magnitude reference counting. This makes it well
// suited for PacketBuffers, which are typically incremented only a few times:
// at creation, when handed off to the TCP package, and when enqueued at a
// socket. As long as there 62 or fewer references taken over the lifetime of
// an object, Refs can increment and decrement the reference count with two
// atomic operations. If there are more than 62, it falls back to a slower,
// mutex-based implementation.
//
// Refs begins as a bitset. Each bit is a ticket: the caller of NewPacketBuffer
// gets ticket 1, the first caller of IncRef gets 2, the next gets 4, etc. It
// also keeps a leftmost "marker bit" to track the next Ticket to distribute.
// So if the bitset looks like this:
//
//   0000....011010.
//
// The meaning is: references 1 and 4 have been passed to DecRef, references 2
// and 8 are live, and the marker bit indicates that 16 is the next Ticket to
// distributes.
//
// The 2 most significant bits are reserved to indicate that too many
// references have been given out, and the counter will "upgrade" to the slower
// implementation. When a call to IncRef acquires the second most significant
// bit (the "upgrading bit") it puts all live Ticket values into the Ticket
// slice, then sets the most significant bit (the "upgraded bit"). Any
// operations on Refs that observe the upgrading bit loop until the upgrade is
// complete. Any operations that observe the upgraded bit use the slow path.
// Because the bitset is modified in a CompareAndSwap loop, the reference count
// maintains a consistent state.
//
// +stateify savable
type Refs struct {
	// bitset holds the first 64 references. If IncRef is called more than that,
	// we upgrade to using the slower ticket system below.
	bitset atomicbitops.Uint64

	// The slow stuff. Only used when we run out of bits in our bitset.
	mu         sync.Mutex
	tickets    []Ticket
	nextTicket uint64
}

// Ticket needs a comment so that the linter doesn't get mad.
// TODO: Which types actually should be public?
type Ticket struct {
	val uint64
}

// InitRefs initializes r with one reference and, if enabled, activates leak
// checking.
func (rf *Refs) InitRefs() Ticket {
	// The least significant 2 bits will be set in the bitset: the first ticket
	// (1) and the marker flag (2).
	ticket := Ticket{1}
	rf.bitset = atomicbitops.FromUint64(0b11)
	refsvfs2.Register(rf)
	// log.Printf("bitset: %#b", rf.bitset)
	return ticket

	// ticket := Ticket{rf.nextTicket}
	// rf.nextTicket++
	// rf.tickets = append(rf.tickets, ticket)
	// refsvfs2.Register(rf)
	// return ticket
}

// RefType implements refsvfs2.CheckedObject.RefType.
func (rf *Refs) RefType() string {
	return "PacketBuffer"
}

// LeakMessage implements refsvfs2.CheckedObject.LeakMessage.
func (rf *Refs) LeakMessage() string {
	return fmt.Sprintf("[%s %p] reference count of %d instead of 0", rf.RefType(), rf, rf.ReadRefs())
}

// LogRefs implements refsvfs2.CheckedObject.LogRefs.
func (rf *Refs) LogRefs() bool {
	return enableLogging
}

// ReadRefs returns the current number of references. The returned count is
// inherently racy and is unsafe to use without external synchronization.
// TODO: mark slow
func (rf *Refs) ReadRefs() int64 {
	for {
		cur := rf.bitset.Load()
		if cur&upgradingState == upgradingState {
			// Yield while waiting for the upgrade, which should be brief.
			runtime.Gosched()
			continue
		}
		if cur&upgradedState == upgradedState {
			rf.mu.Lock()
			defer rf.mu.Unlock()
			return int64(len(rf.tickets))
		}
		return int64(bits.OnesCount64(rf.bitset.Load()) - 1)
	}
}

// If IncRef sees no leading zeros, it provides a mutex ticket. It does NOT
// provide powers of two.
// If DecRef gets something that isn't power of two, it uses mutex tickets.

// IncRef implements refs.RefCounter.IncRef.
//
//go:nosplit
func (rf *Refs) IncRef() Ticket {
	// Fast path.
	for {
		cur := rf.bitset.Load()
		// log.Printf("cur is %#b", cur)
		if cur&upgradingState == upgradingState {
			// Yield while waiting for the upgrade, which should be brief.
			runtime.Gosched()
			continue
		}
		if cur&upgradedState == upgradedState {
			break
		}
		if bits.OnesCount64(cur) <= 1 {
			// The only bit set is the marker bit, so this has a refcount of zero.
			panic(fmt.Sprintf("Incrementing non-positive count %p on %s", rf, rf.RefType()))
		}
		// If the most significant bit is set, we've exhausted the bitset.
		bitsLeft := bits.LeadingZeros64(cur)
		markerVal := uint64(1) << (64 - bitsLeft)
		if !rf.bitset.CompareAndSwap(cur, cur|markerVal) {
			continue
		}
		ticketVal := uint64(1) << (64 - (bitsLeft + 1))
		if markerVal == upgradingState {
			// TODO: This should inc a counter or something.
			// We've given out all 63 tickets. Provide a slow, mutex-based ticket.
			// We pulled the upgrade straw, so now we have to do the upgrade. Blegh.
			rf.mu.Lock()

			// TODO: Preallocate some space.
			rf.tickets = make([]Ticket, 0, 64)
			for i := 0; i < 62; i++ {
				if ticket := (cur | upgradingState) & (uint64(1) << i); ticket != 0 {
					rf.tickets = append(rf.tickets, Ticket{ticket})
				}
			}
			rf.mu.Unlock()

			if !rf.bitset.CompareAndSwap(cur|upgradingState, upgradedState /*|cur|markerVal*/) {
				panic("other goroutines should not race to modify rf.bitset")
			}
			return Ticket{ticketVal}
		}

		// log.Printf("returning ticket val %#b", ticketVal)
		return Ticket{ticketVal}
	}

	// Slow path.
	rf.mu.Lock()

	// We can't give out tickets with a power of two value.
	for bits.OnesCount64(rf.nextTicket) == 1 {
		rf.nextTicket++
	}
	ticket := Ticket{rf.nextTicket}
	rf.nextTicket++
	rf.tickets = append(rf.tickets, ticket)

	if enableLogging {
		refsvfs2.LogIncRef(rf, int64(len(rf.tickets)))
	}

	if len(rf.tickets) <= 1 {
		panic(fmt.Sprintf("Incrementing non-positive count %p on %s", rf, rf.RefType()))
	}

	// log.Printf("returning ticket %d", ticket)
	rf.mu.Unlock()
	return ticket
}

// DecRef implements refs.RefCounter.DecRef.
//
//go:nosplit
func (rf *Refs) DecRef(ticket Ticket, destroy func(PacketBufferPtr), pk PacketBufferPtr) {
	// log.Printf("DecRef: %v", ticket)
	for {
		cur := rf.bitset.Load()
		// log.Printf("cur: %#b", cur)
		if cur&upgradingState == upgradingState {
			// Yield while waiting for the upgrade, which should be brief.
			runtime.Gosched()
			continue
		}
		if cur&upgradedState == upgradedState {
			break
		}
		// // If the most significant bit is set, we've exhausted the bitset.
		// if bits.LeadingZeros64(cur) == bits.LeadingZeros64(ticket.val) {
		// 	panic(fmt.Sprintf("Tried to DecRef with non-existent marker ticket %d, owned by %s", ticket.val, rf.RefType()))
		// }
		if cur&ticket.val != ticket.val {
			panic(fmt.Sprintf("Tried to DecRef with non-existent ticket %d, owned by %s", ticket.val, rf.RefType()))
		}
		if newVal := cur &^ ticket.val; rf.bitset.CompareAndSwap(cur, newVal) {
			// If only the marker flag is left, we've hit zero references.
			if bits.OnesCount64(newVal) == 1 {
				refsvfs2.Unregister(rf)
				// Call the destructor.
				if destroy != nil {
					destroy(pk)
				}
			}
			return
		}
	}

	// Slow path.
	rf.mu.Lock()
	// log.Printf("DecRef state: %v", rf)

	for i, other := range rf.tickets {
		if ticket == other {
			if enableLogging {
				refsvfs2.LogDecRef(rf, int64(len(rf.tickets)))
			}

			rf.tickets = append(rf.tickets[:i], rf.tickets[i+1:]...)
			count := len(rf.tickets)
			rf.mu.Unlock()
			switch {
			case count < 0:
				panic(fmt.Sprintf("Decrementing non-positive ref count %p, owned by %s", rf, rf.RefType()))

			case count == 0:
				refsvfs2.Unregister(rf)
				// Call the destructor.
				if destroy != nil {
					destroy(pk)
				}
			}

			return
		}
	}

	panic(fmt.Sprintf("Tried to DecRef with non-existent ticket %d, owned by %s", ticket.val, rf.RefType()))
}

// func (r *Refs) afterLoad() {
// 	if r.ReadRefs() > 0 {
// 		refsvfs2.Register(r)
// 	}
// }
