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

//go:build amd64
// +build amd64

package seccheck

func init() {
	addSyscallPoint(0, "read", []FieldDesc{
		{
			ID:   FieldSyscallPath,
			Name: "fd_path",
		},
	})
	addSyscallPoint(2, "open", nil)
	addSyscallPoint(3, "close", []FieldDesc{
		{
			ID:   FieldSyscallPath,
			Name: "fd_path",
		},
	})
	addSyscallPoint(42, "connect", []FieldDesc{
		{
			ID:   FieldSyscallPath,
			Name: "fd_path",
		},
	})
	addSyscallPoint(59, "execve", []FieldDesc{
		{
			ID:   FieldExecveEnvv,
			Name: "envv",
		},
	})
	addSyscallPoint(85, "creat", []FieldDesc{
		{
			ID:   FieldSyscallPath,
			Name: "fd_path",
		},
	})
	addSyscallPoint(257, "openat", []FieldDesc{
		{
			ID:   FieldSyscallPath,
			Name: "fd_path",
		},
	})
	addSyscallPoint(322, "execveat", []FieldDesc{
		{
			ID:   FieldSyscallPath,
			Name: "fd_path",
		},
		{
			ID:   FieldExecveEnvv,
			Name: "envv",
		},
	})

	for i := 0; i <= 441; i++ {
		addRawSyscallPoint(uintptr(i))
	}
}
