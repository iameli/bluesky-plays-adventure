package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"

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

func LoadFile(path string) (*SaveGame, error) {
	bs, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	save := SaveGame{}
	err = cbor.Unmarshal(bs, &save)
	if err != nil {
		return nil, err
	}
	return &save, nil
}

type Advent struct {
	Input  chan string
	Output chan string

	StdinReader    io.Reader
	StdinReaderBuf *bufio.Reader
	StdinWriter    io.Writer

	StdoutBuffer []byte

	Started bool
	Loaded  bool
}

type AdventWriter struct{}

func NewAdvent() (*Advent, error) {
	reader, writer := io.Pipe()
	readerBuf := bufio.NewReader(reader)
	a := Advent{
		Input:          make(chan string),
		Output:         make(chan string),
		StdinReader:    reader,
		StdinReaderBuf: readerBuf,
		StdinWriter:    writer,
		StdoutBuffer:   []byte{},
		Started:        false,
		Loaded:         false,
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
	mem := mod.Memory()
	// if this is our first prompt ever, answer "no" to get us to the right code path
	// answer "no" to "do you want instructions" so we can go immediately to the right code path
	if !a.Started {
		go func() {
			a.StdinWriter.Write([]byte("no\n"))
			a.Started = true
		}()
	} else {
		// stdin requested so dump the stdout buffer if present
		if len(a.StdoutBuffer) > 0 {
			a.Output <- string(a.StdoutBuffer)
			a.StdoutBuffer = []byte{}
		}
		if !a.Loaded {
			// if we've loaded to the main loop and haven't loaded our file yet, load game
			save, err := LoadFile("save.cbor")
			if err != nil {
				panic(fmt.Errorf("error loading file: %s", err))
			}
			mem.Write(0, save.Memory)
			iovs = int32(save.StdinAddr)
			iovsCount = int32(save.StdinCount)
			resultNread = int32(save.ResultAddr)
			a.Loaded = true
		} else {
			// otherwise save the game so we can come back here later
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
	}

	ret := int32(a.fdRead(mod, []int32{fd, iovs, iovsCount, resultNread}))
	return ret
}

func (a *Advent) Start(ctx context.Context) error {
	// Create a new WebAssembly Runtime.
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)

	config := wazero.NewModuleConfig().WithStdin(a.StdinReader).WithStdout(a).WithStderr(a)

	builder := r.NewHostModuleBuilder("wasi_snapshot_preview1")
	wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(builder)

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

func (a *Advent) stdinReader(buf []byte) (n int, errno experimentalsys.Errno) {
	b, err := a.StdinReaderBuf.ReadByte()
	if err != nil {
		panic(err)
	}
	buf[0] = b
	return 1, 0
}

func (a *Advent) fdRead(mod api.Module, params []int32) experimentalsys.Errno {
	mem := mod.Memory()

	fd := int32(params[0])
	if fd != 0 {
		panic("only know how to read stdin")
	}
	iovs := uint32(params[1])
	iovsCount := uint32(params[2])

	resultNread := uint32(params[3])

	nread, errno := a.readv(mem, iovs, iovsCount, a.stdinReader)
	if errno != 0 {
		return errno
	}
	if !mem.WriteUint32Le(resultNread, nread) {
		return experimentalsys.EFAULT
	} else {
		return 0
	}
}

func (a *Advent) readv(mem api.Memory, iovs uint32, iovsCount uint32, reader func(buf []byte) (nread int, errno experimentalsys.Errno)) (uint32, experimentalsys.Errno) {
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
