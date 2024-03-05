package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"

	_ "embed"

	"github.com/fxamacker/cbor"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	experimentalsys "github.com/tetratelabs/wazero/experimental/sys"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed adv550.wasm
var adv550 []byte

// All data necessary for a save state
type SaveGame struct {
	Memory     []byte `cbor:"memory"`
	StdinAddr  int64  `cbor:"stdinaddr"`
	StdinCount int64  `cbor:"stdincount"`
	ResultAddr int64  `cbor:"resultaddr"`
}

func (s *SaveGame) SaveFile(path string) error {
	raw, err := cbor.Marshal(s, cbor.EncOptions{})
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0644)
}

type Advent struct {
	Input  chan string
	Output chan string

	StdinReader io.Reader
	StdinWriter io.Writer

	StdoutBuffer []byte
}

type AdventWriter struct{}

func NewAdvent() (*Advent, error) {
	reader, writer := io.Pipe()
	a := Advent{
		Input:        make(chan string),
		Output:       make(chan string),
		StdinReader:  reader,
		StdinWriter:  writer,
		StdoutBuffer: []byte{},
	}
	return &a, nil
}

// implementing io.Writer is a simple way to get stdout written to us
func (a *Advent) Write(p []byte) (int, error) {
	a.StdoutBuffer = append(a.StdoutBuffer, p...)
	return len(p), nil
}

// custom read(2) implementation where we inject most of our magic - called when the app wants stdin
func (a *Advent) CRead(ctx context.Context, mod api.Module, fd, iovs, iovsCount, resultNread int32) int32 {
	// stdin requested so dump the stdout buffer if present
	if len(a.StdoutBuffer) > 0 {
		a.Output <- string(a.StdoutBuffer)
		a.StdoutBuffer = []byte{}

		// next do a savestate so we can come back here later
		mem := mod.Memory()
		bs, ok := mem.Read(0, mem.Size())
		if !ok {
			panic("not ok reading wasm memory")
		}
		state := &SaveGame{
			Memory:     bs,
			StdinAddr:  int64(iovs),
			StdinCount: int64(iovsCount),
			ResultAddr: int64(resultNread),
		}
		err := state.SaveFile("save.cbor")
		if err != nil {
			panic(fmt.Errorf("error saving file: %s", err))
		}
	}

	ret := int32(fdRead(mod, []int32{fd, iovs, iovsCount, resultNread}))
	return ret
}

func (a *Advent) Start(ctx context.Context) error {
	// Create a new WebAssembly Runtime.
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)

	config := wazero.NewModuleConfig().WithStdin(a.StdinReader).WithStdout(a).WithStderr(a)

	builder := r.NewHostModuleBuilder("wasi_snapshot_preview1")
	wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(builder)

	// answer "no" to "do you want instructions" so we can go immediately to the right code path
	go func() {
		a.StdinWriter.Write([]byte("no\n"))
	}()

	builder.NewFunctionBuilder().WithFunc(a.CRead).Export("fd_read")

	builder.Instantiate(ctx)
	// Instantiate the guest Wasm into the same runtime. It exports the `add`
	// function, implemented in WebAssembly.
	_, err := r.InstantiateWithConfig(ctx, adv550, config)
	if err != nil {
		return fmt.Errorf("failed to instantiate module: %v", err)
	}

	return nil
}

var le = binary.LittleEndian
var start *bufio.Reader
var sr *bufio.Reader

func stdinReader(buf []byte) (n int, errno experimentalsys.Errno) {
	if start == nil {
		start = bufio.NewReader(strings.NewReader("no\n"))
	}
	_, err := start.Peek(1)
	if err == nil {
		b, err := start.ReadByte()
		if err != nil {
			panic(err)
		}
		buf[0] = b
		return 1, 0
	}
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
