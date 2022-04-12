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

// PacketBufferList is TODO: NOT an intrusive list. Entries can be added to
// or removed from the list in O(1) time and with no additional memory
// allocations.
//
// The zero value for List is an empty list ready to use.
//
// To iterate over a list (where l is a List):
//      for e := l.Front(); e != nil; e = e.Next() {
// 		// do something with e.
//      }
//
// +stateify savable
type PacketBufferList struct {
	// TODO: backing buffer
	pbs []PacketBufferPtr
}

// AsSlice returns a slice containing the packets in pl. Modifying the slice
// does not modify pl, but modifying the underlying PacketBuffers does.
func (pl *PacketBufferList) AsSlice() []PacketBufferPtr {
	return pl.pbs
}

// Reset resets list l to the empty state.
func (pl *PacketBufferList) Reset() {
	pl.pbs = nil
}

// Empty returns true iff the list is empty.
//
//go:nosplit
func (pl *PacketBufferList) Empty() bool {
	return len(pl.pbs) > 0
}

// Front returns the first element of list l or nil.
//
//go:nosplit
func (pl *PacketBufferList) Front() PacketBufferPtr {
	return pl.pbs[0]
}

// Back returns the last element of list l or nil.
//
//go:nosplit
func (pl *PacketBufferList) Back() PacketBufferPtr {
	return pl.pbs[len(pl.pbs)-1]
}

// Len returns the number of elements in the list.
//
// NOTE: This is an O(n) operation.
//
//go:nosplit
func (pl *PacketBufferList) Len() int {
	return len(pl.pbs)
}

// PushFront inserts the element e at the front of list l.
//
//go:nosplit
func (pl *PacketBufferList) PushFront(pb PacketBufferPtr) {
	pl.pbs = append([]PacketBufferPtr{pb}, pl.pbs...)
}

// PushBack inserts the element e at the back of list l.
//
//go:nosplit
func (pl *PacketBufferList) PushBack(pb PacketBufferPtr) {
	pl.pbs = append(pl.pbs, pb)
}

// Remove removes e from l.
//
// TODO: This and maybe other methods would benefit from changing, as users
// expect O(1) run time. E.g. this should really be RemoveFront(), which is
// faster and is the only way in which this method is used.
//
//go:nosplit
func (pl *PacketBufferList) Remove(pb PacketBufferPtr) {
	for i := range pl.pbs {
		if pl.pbs[i] == pb {
			pl.pbs = append(pl.pbs[:i], pl.pbs[i+1:]...)
			return
		}
	}
}

// IncRef increases the reference count on each PacketBuffer
// stored in the PacketBufferList.
func (pl *PacketBufferList) IncRef() {
	for _, pb := range pl.pbs {
		pb.IncRef()
	}
}

// DecRef decreases the reference count on each PacketBuffer
// stored in the PacketBufferList.
func (pl PacketBufferList) DecRef() {
	for _, pb := range pl.pbs {
		pb.DecRef()
	}
}
