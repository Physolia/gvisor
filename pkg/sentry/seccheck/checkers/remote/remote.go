// Copyright 2021 The gVisor Authors.
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

// Package remote ...
package remote

import (
	"fmt"
	"os"

	"gvisor.dev/gvisor/pkg/log"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"gvisor.dev/gvisor/pkg/cleanup"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/sentry/seccheck"

	"google.golang.org/protobuf/types/known/anypb"
	pb "gvisor.dev/gvisor/pkg/sentry/seccheck/points/points_go_proto"
)

func init() {
	seccheck.RegisterSink(seccheck.SinkDesc{
		Name:  "remote",
		Setup: Setup,
		New:   New,
	})
}

type Remote struct {
	seccheck.CheckerDefaults

	endpoint *fd.FD
}

var _ seccheck.Checker = (*Remote)(nil)

func Setup(config map[string]interface{}) (*os.File, error) {
	addrOpaque, ok := config["endpoint"]
	if !ok {
		return nil, fmt.Errorf("endpoint not present in configuration")
	}
	addr, ok := addrOpaque.(string)
	if !ok {
		return nil, fmt.Errorf("endpoint %q is not a string", addrOpaque)
	}
	return setup(addr)
}

func setup(path string) (*os.File, error) {
	log.Debugf("Remote sink connecting to %q", path)
	socket, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(socket), path)
	cu := cleanup.Make(func() {
		_ = f.Close()
	})
	defer cu.Clean()

	addr := unix.SockaddrUnix{Name: path}
	if err := unix.Connect(int(f.Fd()), &addr); err != nil {
		return nil, err
	}
	cu.Release()
	return f, nil
}

func New(_ map[string]interface{}, endpoint *fd.FD) (seccheck.Checker, error) {
	if endpoint == nil {
		return nil, fmt.Errorf("remote sink requires an endpoint")
	}
	return &Remote{endpoint: endpoint}, nil
}

// Header ...
//
// +marshal
type Header struct {
	MessageSize  uint32
	HeaderSize   uint16 // Doesn't include MessageSize.
	DroppedCount uint32 `marshal:"unaligned"`
}

// Note: Any requires writing the full type URL to the message. We're not
// memory bandwidth bound, but having an enum event type in the header to
// identify the proto type would reduce message size and speed up event dispatch
// in the consumer.
func (r *Remote) writeAny(any *anypb.Any) error {
	out, err := proto.Marshal(any)
	if err != nil {
		return err
	}
	const headerLength = 10
	hdr := Header{
		MessageSize: uint32(len(out) + headerLength),
		HeaderSize:  uint16(headerLength - 4),
	}
	var hdrOut [headerLength]byte
	hdr.MarshalUnsafe(hdrOut[:])

	// TODO(fvoznika): No blocking write. Count as dropped if write partial.
	_, err = unix.Writev(r.endpoint.FD(), [][]byte{hdrOut[:], out})
	return err
}

func (r *Remote) write(msg proto.Message) {
	any, err := anypb.New(msg)
	if err != nil {
		log.Debugf("anypd.New(%+v): %v", msg, err)
		return
	}
	if err := r.writeAny(any); err != nil {
		log.Debugf("writeAny(%+v): %v", any, err)
		return
	}
	return
}

func (r *Remote) RawSyscall(_ context.Context, _ seccheck.FieldSet, info *pb.Syscall) error {
	r.write(info)
	return nil
}

func (r *Remote) Syscall(ctx context.Context, fields seccheck.FieldSet, cb seccheck.SyscallToProto, common *pb.Common, info seccheck.SyscallInfo) error {
	r.write(cb(ctx, fields, common, info))
	return nil
}

func (r *Remote) ContainerStart(_ context.Context, _ seccheck.FieldSet, info *pb.Start) error {
	r.write(info)
	return nil
}

func (r *Remote) TaskExit(_ context.Context, _ seccheck.FieldSet, info *pb.TaskExit) error {
	r.write(info)
	return nil
}

func (r *Remote) Clone(_ context.Context, _ seccheck.FieldSet, info *pb.CloneInfo) error {
	r.write(info)
	return nil
}

func (r *Remote) Execve(_ context.Context, _ seccheck.FieldSet, info *pb.ExecveInfo) error {
	r.write(info)
	return nil
}

func (r *Remote) ExitNotifyParent(_ context.Context, _ seccheck.FieldSet, info *pb.ExitNotifyParentInfo) error {
	r.write(info)
	return nil
}
