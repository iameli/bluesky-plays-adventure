package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"os"

	_ "embed"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	experimentalsys "github.com/tetratelabs/wazero/experimental/sys"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	_ "github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed adv550.wasm
var adv550 []byte

func StartAdventWASM(ctx context.Context) error {
	ctx = context.WithValue(ctx, experimental.EnableSnapshotterKey{}, struct{}{})
	// Create a new WebAssembly Runtime.
	r := wazero.NewRuntime(ctx)

	config := wazero.NewModuleConfig().WithStdin(os.Stdin).WithStdout(os.Stdout).WithStderr(os.Stderr)

	// var fdRead = newHostFunc(
	// 	wasip1.FdReadName, fdReadFn,
	// 	[]api.ValueType{i32, i32, i32, i32},
	// 	"fd", "iovs", "iovs_len", "result.nread",
	// )

	// read := func(_ context.Context, mod api.Module, params []uint64) uint16 {
	// 	return 0
	// }
	// wazero.HostFunctionBuilder

	// fdRead := &api.HostFunc{
	// 	ExportName:  "fd_read",
	// 	Name:        "fd_read",
	// 	ParamTypes:  []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
	// 	ParamNames:  []string{"fd", "iovs", "iovs_len", "result.nread"},
	// 	ResultTypes: []api.ValueType{api.ValueTypeI32},
	// 	ResultNames: []string{"errno"},
	// 	Code:        api.Code{GoFunc: read},
	// }

	// x := &api.

	defer r.Close(ctx) // This closes everything this Runtime created.

	// Instantiate WASI, which implements host functions needed for TinyGo to
	// implement `panic`.

	builder := r.NewHostModuleBuilder("wasi_snapshot_preview1")
	wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(builder)

	builder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, a, b, c, d int32) int32 {
			fmt.Printf("%d %d %d %d\n", a, b, c, d)
			// snapshotter := ctx.Value(experimental.SnapshotterKey{}).(experimental.Snapshotter)
			// _ = snapshotter.Snapshot()
			// fmt.Println(snapshot)
			// panic("got here")
			// return 0
			return int32(fdRead(mod, []int32{a, b, c, d}))
		}).Export("fd_read")

	// _, err := r.NewHostModuleBuilder("env").
	// 	NewFunctionBuilder().
	// 	WithFunc().
	// 	Export("fd_read").
	// 	Instantiate(ctx)
	// if err != nil {
	// 	return fmt.Errorf("failed to instantiate module: %v", err)
	// }

	builder.Instantiate(ctx)
	// Instantiate the guest Wasm into the same runtime. It exports the `add`
	// function, implemented in WebAssembly.
	_, err := r.InstantiateWithConfig(ctx, adv550, config)
	if err != nil {
		return fmt.Errorf("failed to instantiate module: %v", err)
	}

	return nil

	// // Read two args to add.
	// x, y, err := readTwoArgs(flag.Arg(0), flag.Arg(1))
	// if err != nil {
	// 	log.Panicf("failed to read arguments: %v", err)
	// }

	// // Call the `add` function and print the results to the console.
	// add := mod.ExportedFunction("add")
	// results, err := add.Call(ctx, x, y)
	// if err != nil {
	// 	log.Panicf("failed to call add: %v", err)
	// }

	// fmt.Printf("%d + %d = %d\n", x, y, results[0])
}

var le = binary.LittleEndian
var sr *bufio.Reader

func stdinReader(buf []byte) (n int, errno experimentalsys.Errno) {
	if sr == nil {
		sr = bufio.NewReader(os.Stdin)
	}
	b, err := sr.ReadByte()
	if err != nil {
		panic(err)
	}
	buf[0] = b
	return 1, 0
}

func fdRead(mod api.Module, params []int32) experimentalsys.Errno {
	mem := mod.Memory()

	fd := int32(params[0])
	if fd != 0 {
		panic("only know how to read stdin")
	}
	iovs := uint32(params[1])
	iovsCount := uint32(params[2])

	resultNread := uint32(params[3])

	nread, errno := readv(mem, iovs, iovsCount, stdinReader)
	if errno != 0 {
		return errno
	}
	if !mem.WriteUint32Le(resultNread, nread) {
		return experimentalsys.EFAULT
	} else {
		return 0
	}
}

func readv(mem api.Memory, iovs uint32, iovsCount uint32, reader func(buf []byte) (nread int, errno experimentalsys.Errno)) (uint32, experimentalsys.Errno) {
	var nread uint32
	iovsStop := iovsCount << 3 // iovsCount * 8
	iovsBuf, ok := mem.Read(iovs, iovsStop)
	if !ok {
		return 0, experimentalsys.EFAULT
	}

	for iovsPos := uint32(0); iovsPos < iovsStop; iovsPos += 8 {
		offset := le.Uint32(iovsBuf[iovsPos:])
		l := le.Uint32(iovsBuf[iovsPos+4:])

		if l == 0 { // A zero length iovec could be ahead of another.
			continue
		}

		b, ok := mem.Read(offset, l)
		if !ok {
			return 0, experimentalsys.EFAULT
		}

		n, errno := reader(b)
		nread += uint32(n)

		if errno == experimentalsys.ENOSYS {
			return 0, experimentalsys.EBADF // e.g. unimplemented for read
		} else if errno != 0 {
			return 0, errno
		} else if n < int(l) {
			break // stop when we read less than capacity.
		}
	}
	return nread, 0
}
