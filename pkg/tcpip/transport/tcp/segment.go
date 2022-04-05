// Copyright 2018 The gVisor Authors.
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

package tcp

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/seqnum"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// queueFlags are used to indicate which queue of an endpoint a particular segment
// belongs to. This is used to track memory accounting correctly.
type queueFlags uint8

const (
	// SegOverheadSize is the size of an empty seg in memory including packet
	// buffer overhead. It is advised to used SegOverheadSize instead of segSize
	// in all cases where accounting for segment memory overhead is important.
	SegOverheadSize            = segSize + stack.PacketBufferStructSize
	recvQ           queueFlags = 1 << iota
	sendQ
)

// segment represents a TCP segment. It holds the payload and parsed TCP segment
// information, and can be added to intrusive lists.
// segment is mostly immutable, the only field allowed to change is data.
//
// +stateify savable
type segment struct {
	segmentEntry
	segmentRefs

	ep     *endpoint
	qFlags queueFlags
	id     stack.TransportEndpointID `state:"manual"`

	// TODO(gvisor.dev/issue/4417): Hold a stack.PacketBuffer instead of
	// individual members for link/network packet info.
	srcAddr  tcpip.Address
	dstAddr  tcpip.Address
	netProto tcpip.NetworkProtocolNumber
	nicID    tcpip.NICID

	pkt *stack.PacketBuffer

	sequenceNumber seqnum.Value
	ackNumber      seqnum.Value
	flags          header.TCPFlags
	window         seqnum.Size
	// csum is only populated for received segments.
	csum uint16
	// csumValid is true if the csum in the received segment is valid.
	csumValid bool

	// parsedOptions stores the parsed values from the options in the segment.
	parsedOptions  header.TCPOptions
	options        []byte `state:".([]byte)"`
	hasNewSACKInfo bool
	rcvdTime       tcpip.MonotonicTime
	// xmitTime is the last transmit time of this segment.
	xmitTime  tcpip.MonotonicTime
	xmitCount uint32

	// acked indicates if the segment has already been SACKed.
	acked bool

	// dataMemSize is the memory used by data initially.
	dataMemSize int

	// lost indicates if the segment is marked as lost by RACK.
	lost bool
}

func newIncomingSegment(id stack.TransportEndpointID, clock tcpip.Clock, pkt *stack.PacketBuffer) *segment {
	netHdr := pkt.Network()
	s := &segment{
		id:       id,
		srcAddr:  netHdr.SourceAddress(),
		dstAddr:  netHdr.DestinationAddress(),
		netProto: pkt.NetworkProtocolNumber,
		nicID:    pkt.NICID,
	}
	s.InitRefs()
	pkt.IncRef()
	s.pkt = pkt
	s.rcvdTime = clock.NowMonotonic()
	s.dataMemSize = s.pkt.MemSize()
	return s
}

func newOutgoingSegment(id stack.TransportEndpointID, clock tcpip.Clock, v buffer.View) *segment {
	s := &segment{
		id: id,
	}
	s.InitRefs()
	s.rcvdTime = clock.NowMonotonic()
	s.pkt = stack.NewPacketBuffer(stack.PacketBufferOptions{})
	s.pkt.Data().AppendView(v)
	s.dataMemSize = s.pkt.MemSize()
	return s
}

// clone creates a shallow clone of s not including its pkt.
func (s *segment) clone() *segment {
	t := &segment{
		id:             s.id,
		sequenceNumber: s.sequenceNumber,
		ackNumber:      s.ackNumber,
		flags:          s.flags,
		window:         s.window,
		netProto:       s.netProto,
		nicID:          s.nicID,
		rcvdTime:       s.rcvdTime,
		xmitTime:       s.xmitTime,
		xmitCount:      s.xmitCount,
		ep:             s.ep,
		qFlags:         s.qFlags,
		dataMemSize:    s.dataMemSize,
	}
	t.InitRefs()
	t.pkt = stack.NewPacketBuffer(stack.PacketBufferOptions{})
	return t
}

// merge merges data in oth and clears oth.
func (s *segment) merge(oth *segment) {
	s.pkt.Data().ReadFrom(oth.pkt.Data(), oth.pkt.Data().Size())
	s.dataMemSize = s.pkt.MemSize()
	oth.dataMemSize = oth.pkt.MemSize()
}

// setOwner sets the owning endpoint for this segment. Its required
// to be called to ensure memory accounting for receive/send buffer
// queues is done properly.
func (s *segment) setOwner(ep *endpoint, qFlags queueFlags) {
	switch qFlags {
	case recvQ:
		ep.updateReceiveMemUsed(s.segMemSize())
	case sendQ:
		// no memory account for sendQ yet.
	default:
		panic(fmt.Sprintf("unexpected queue flag %b", qFlags))
	}
	s.ep = ep
	s.qFlags = qFlags
}

func (s *segment) DecRef() {
	s.segmentRefs.DecRef(func() {
		if s.ep != nil {
			switch s.qFlags {
			case recvQ:
				s.ep.updateReceiveMemUsed(-s.segMemSize())
			case sendQ:
				// no memory accounting for sendQ yet.
			default:
				panic(fmt.Sprintf("unexpected queue flag %b set for segment", s.qFlags))
			}
		}
		s.pkt.DecRef()
	})
}

// logicalLen is the segment length in the sequence number space. It's defined
// as the data length plus one for each of the SYN and FIN bits set.
func (s *segment) logicalLen() seqnum.Size {
	l := seqnum.Size(s.payloadSize())
	if s.flags.Contains(header.TCPFlagSyn) {
		l++
	}
	if s.flags.Contains(header.TCPFlagFin) {
		l++
	}
	return l
}

// payloadSize is the size of s.data.
func (s *segment) payloadSize() int {
	return s.pkt.Data().Size()
}

// segMemSize is the amount of memory used to hold the segment data and
// the associated metadata.
func (s *segment) segMemSize() int {
	return segSize + s.dataMemSize
}

// parse populates the sequence & ack numbers, flags, and window fields of the
// segment from a TCP header. It then updates the view to skip the header.
//
// Returns boolean indicating if the parsing was successful.
//
// If checksum verification may not be skipped, parse also verifies the
// TCP checksum and stores the checksum and result of checksum verification in
// the csum and csumValid fields of the segment.
func (s *segment) parse(hdr header.TCP, skipChecksumValidation bool) bool {
	// h is the header followed by the payload. We check that the offset to
	// the data respects the following constraints:
	// 1. That it's at least the minimum header size; if we don't do this
	//    then part of the header would be delivered to user.
	// 2. That the header fits within the buffer; if we don't do this, we
	//    would panic when we tried to access data beyond the buffer.
	//
	// N.B. The segment has already been validated as having at least the
	//      minimum TCP size before reaching here, so it's safe to read the
	//      fields.
	offset := int(hdr.DataOffset())
	if offset < header.TCPMinimumSize || offset > len(hdr) {
		return false
	}

	s.options = hdr[header.TCPMinimumSize:]
	s.parsedOptions = header.ParseTCPOptions(s.options)
	if skipChecksumValidation {
		s.csumValid = true
	} else {
		s.csum = hdr.Checksum()
		payloadChecksum := s.pkt.Data().AsRange().Checksum()
		payloadLength := uint16(s.payloadSize())
		s.csumValid = hdr.IsChecksumValid(s.srcAddr, s.dstAddr, payloadChecksum, payloadLength)
	}
	s.sequenceNumber = seqnum.Value(hdr.SequenceNumber())
	s.ackNumber = seqnum.Value(hdr.AckNumber())
	s.flags = hdr.Flags()
	s.window = seqnum.Size(hdr.WindowSize())
	return true
}

// sackBlock returns a header.SACKBlock that represents this segment.
func (s *segment) sackBlock() header.SACKBlock {
	return header.SACKBlock{Start: s.sequenceNumber, End: s.sequenceNumber.Add(s.logicalLen())}
}
