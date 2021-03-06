// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

// This file does serialization of programs for executor binary.
// The format aims at simple parsing: binary and irreversible.

// Exec format is an sequence of uint64's which encodes a sequence of calls.
// The sequence is terminated by a speciall call execInstrEOF.
// Each call is (call ID, copyout index, number of arguments, arguments...).
// Each argument is (type, size, value).
// There are 4 types of arguments:
//  - execArgConst: value is const value
//  - execArgResult: value is copyout index we want to reference
//  - execArgData: value is a binary blob (represented as ]size/8[ uint64's)
//  - execArgCsum: runtime checksum calculation
// There are 2 other special calls:
//  - execInstrCopyin: copies its second argument into address specified by first argument
//  - execInstrCopyout: reads value at address specified by first argument (result can be referenced by execArgResult)

package prog

import (
	"fmt"
	"sort"
)

const (
	execInstrEOF = ^uint64(iota)
	execInstrCopyin
	execInstrCopyout
)

const (
	execArgConst = uint64(iota)
	execArgResult
	execArgData
	execArgCsum
)

const (
	ExecArgCsumInet = uint64(iota)
)

const (
	ExecArgCsumChunkData = uint64(iota)
	ExecArgCsumChunkConst
)

const (
	ExecBufferSize = 2 << 20
	ExecNoCopyout  = ^uint64(0)
)

type Args []Arg

func (s Args) Len() int {
	return len(s)
}

func (s Args) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

type ByPhysicalAddr struct {
	Args
	Context *execContext
}

func (s ByPhysicalAddr) Less(i, j int) bool {
	return s.Context.args[s.Args[i]].Addr < s.Context.args[s.Args[j]].Addr
}

// SerializeForExec serializes program p for execution by process pid into the provided buffer.
// Returns number of bytes written to the buffer.
// If the provided buffer is too small for the program an error is returned.
func (p *Prog) SerializeForExec(buffer []byte, pid int) (int, error) {
	if debug {
		if err := p.validate(); err != nil {
			panic(fmt.Errorf("serializing invalid program: %v", err))
		}
	}
	var copyoutSeq uint64
	w := &execContext{
		target: p.Target,
		buf:    buffer,
		eof:    false,
		args:   make(map[Arg]argInfo),
	}
	for _, c := range p.Calls {
		// Calculate checksums.
		csumMap := calcChecksumsCall(c, pid)
		var csumUses map[Arg]bool
		if csumMap != nil {
			csumUses = make(map[Arg]bool)
			for arg, info := range csumMap {
				csumUses[arg] = true
				if info.Kind == CsumInet {
					for _, chunk := range info.Chunks {
						if chunk.Kind == CsumChunkArg {
							csumUses[chunk.Arg] = true
						}
					}
				}
			}
		}
		// Calculate arg offsets within structs.
		// Generate copyin instructions that fill in data into pointer arguments.
		foreachArg(c, func(arg, _ Arg, _ *[]Arg) {
			if a, ok := arg.(*PointerArg); ok && a.Res != nil {
				foreachSubargOffset(a.Res, func(arg1 Arg, offset uint64) {
					addr := p.Target.physicalAddr(arg) + offset
					if isUsed(arg1) || csumUses[arg1] {
						w.args[arg1] = argInfo{Addr: addr}
					}
					if _, ok := arg1.(*GroupArg); ok {
						return
					}
					if _, ok := arg1.(*UnionArg); ok {
						return
					}
					if a1, ok := arg1.(*DataArg); ok &&
						(a1.Type().Dir() == DirOut || len(a1.Data()) == 0) {
						return
					}
					if !IsPad(arg1.Type()) && arg1.Type().Dir() != DirOut {
						w.write(execInstrCopyin)
						w.write(addr)
						w.writeArg(arg1, pid)
					}
				})
			}
		})
		// Generate checksum calculation instructions starting from the last one,
		// since checksum values can depend on values of the latter ones
		if csumMap != nil {
			var csumArgs []Arg
			for arg := range csumMap {
				csumArgs = append(csumArgs, arg)
			}
			sort.Sort(ByPhysicalAddr{Args: csumArgs, Context: w})
			for i := len(csumArgs) - 1; i >= 0; i-- {
				arg := csumArgs[i]
				if _, ok := arg.Type().(*CsumType); !ok {
					panic("csum arg is not csum type")
				}
				w.write(execInstrCopyin)
				w.write(w.args[arg].Addr)
				w.write(execArgCsum)
				w.write(arg.Size())
				switch csumMap[arg].Kind {
				case CsumInet:
					w.write(ExecArgCsumInet)
					w.write(uint64(len(csumMap[arg].Chunks)))
					for _, chunk := range csumMap[arg].Chunks {
						switch chunk.Kind {
						case CsumChunkArg:
							w.write(ExecArgCsumChunkData)
							w.write(w.args[chunk.Arg].Addr)
							w.write(chunk.Arg.Size())
						case CsumChunkConst:
							w.write(ExecArgCsumChunkConst)
							w.write(chunk.Value)
							w.write(chunk.Size)
						default:
							panic(fmt.Sprintf("csum chunk has unknown kind %v", chunk.Kind))
						}
					}
				default:
					panic(fmt.Sprintf("csum arg has unknown kind %v", csumMap[arg].Kind))
				}
			}
		}
		// Generate the call itself.
		w.write(uint64(c.Meta.ID))
		if isUsed(c.Ret) {
			w.args[c.Ret] = argInfo{Idx: copyoutSeq}
			w.write(copyoutSeq)
			copyoutSeq++
		} else {
			w.write(ExecNoCopyout)
		}
		w.write(uint64(len(c.Args)))
		for _, arg := range c.Args {
			w.writeArg(arg, pid)
		}
		// Generate copyout instructions that persist interesting return values.
		foreachArg(c, func(arg, base Arg, _ *[]Arg) {
			if !isUsed(arg) {
				return
			}
			switch arg.(type) {
			case *ReturnArg:
				// Idx is already assigned above.
			case *ConstArg, *ResultArg:
				// Create a separate copyout instruction that has own Idx.
				if _, ok := base.(*PointerArg); !ok {
					panic("arg base is not a pointer")
				}
				info := w.args[arg]
				info.Idx = copyoutSeq
				copyoutSeq++
				w.args[arg] = info
				w.write(execInstrCopyout)
				w.write(info.Idx)
				w.write(info.Addr)
				w.write(arg.Size())
			default:
				panic("bad arg kind in copyout")
			}
		})
	}
	w.write(execInstrEOF)
	if w.eof {
		return 0, fmt.Errorf("provided buffer is too small")
	}
	return len(buffer) - len(w.buf), nil
}

func (target *Target) physicalAddr(arg Arg) uint64 {
	a, ok := arg.(*PointerArg)
	if !ok {
		panic("physicalAddr: bad arg kind")
	}
	addr := a.PageIndex*target.PageSize + target.DataOffset
	if a.PageOffset >= 0 {
		addr += uint64(a.PageOffset)
	} else {
		addr += target.PageSize - uint64(-a.PageOffset)
	}
	return addr
}

type execContext struct {
	target *Target
	buf    []byte
	eof    bool
	args   map[Arg]argInfo
}

type argInfo struct {
	Addr uint64 // physical addr
	Idx  uint64 // copyout instruction index
}

func (w *execContext) write(v uint64) {
	if len(w.buf) < 8 {
		w.eof = true
		return
	}
	w.buf[0] = byte(v >> 0)
	w.buf[1] = byte(v >> 8)
	w.buf[2] = byte(v >> 16)
	w.buf[3] = byte(v >> 24)
	w.buf[4] = byte(v >> 32)
	w.buf[5] = byte(v >> 40)
	w.buf[6] = byte(v >> 48)
	w.buf[7] = byte(v >> 56)
	w.buf = w.buf[8:]
}

func (w *execContext) writeArg(arg Arg, pid int) {
	switch a := arg.(type) {
	case *ConstArg:
		w.write(execArgConst)
		w.write(a.Size())
		w.write(a.Value(pid))
		w.write(a.Type().BitfieldOffset())
		w.write(a.Type().BitfieldLength())
	case *ResultArg:
		if a.Res == nil {
			w.write(execArgConst)
			w.write(a.Size())
			w.write(a.Val)
			w.write(0) // bit field offset
			w.write(0) // bit field length
		} else {
			info, ok := w.args[a.Res]
			if !ok {
				panic("no copyout index")
			}
			w.write(execArgResult)
			w.write(a.Size())
			w.write(info.Idx)
			w.write(a.OpDiv)
			w.write(a.OpAdd)
		}
	case *PointerArg:
		w.write(execArgConst)
		w.write(a.Size())
		w.write(w.target.physicalAddr(arg))
		w.write(0) // bit field offset
		w.write(0) // bit field length
	case *DataArg:
		data := a.Data()
		w.write(execArgData)
		w.write(uint64(len(data)))
		padded := len(data)
		if pad := 8 - len(data)%8; pad != 8 {
			padded += pad
		}
		if len(w.buf) < padded {
			w.eof = true
		} else {
			copy(w.buf, data)
			w.buf = w.buf[padded:]
		}
	default:
		panic("unknown arg type")
	}
}
