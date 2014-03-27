// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package server provides RPC access to a local program being debugged.
// It is the remote end of the client implementation of the Program interface.
package server

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"debug/elf"
	"debug/macho"
	"debug/pe"

	"code.google.com/p/ogle/gosym"
	"code.google.com/p/ogle/program"
	"code.google.com/p/ogle/program/proxyrpc"
)

type breakpoint struct {
	pc        uint64
	origInstr byte // TODO: don't be amd64-specific.
}

type Server struct {
	executable string // Name of executable.
	lines      *gosym.LineTable
	symbols    *gosym.Table

	mu sync.Mutex

	fc chan func() error
	ec chan error

	proc        *os.Process
	breakpoints map[uint64]breakpoint
	files       []*file // Index == file descriptor.
}

// New parses the executable and builds local data structures for answering requests.
// It returns a Server ready to serve requests about the executable.
func New(executable string) (*Server, error) {
	fd, err := os.Open(executable)
	if err != nil {
		return nil, err
	}
	defer fd.Close()
	textStart, symtab, pclntab, err := loadTables(fd)
	if err != nil {
		return nil, err
	}
	lines := gosym.NewLineTable(pclntab, textStart)
	symbols, err := gosym.NewTable(symtab, lines)
	if err != nil {
		return nil, err
	}
	srv := &Server{
		executable:  executable,
		lines:       lines,
		symbols:     symbols,
		fc:          make(chan func() error),
		ec:          make(chan error),
		breakpoints: make(map[uint64]breakpoint),
	}
	go ptraceRun(srv.fc, srv.ec)
	return srv, nil
}

// This function is copied from $GOROOT/src/cmd/addr2line/main.go.
// TODO: Make this architecture-defined? Push into gosym?
// TODO: Why is the .gosymtab always empty?
func loadTables(f *os.File) (textStart uint64, symtab, pclntab []byte, err error) {
	if obj, err := elf.NewFile(f); err == nil {
		if sect := obj.Section(".text"); sect != nil {
			textStart = sect.Addr
		}
		if sect := obj.Section(".gosymtab"); sect != nil {
			if symtab, err = sect.Data(); err != nil {
				return 0, nil, nil, err
			}
		}
		if sect := obj.Section(".gopclntab"); sect != nil {
			if pclntab, err = sect.Data(); err != nil {
				return 0, nil, nil, err
			}
		}
		return textStart, symtab, pclntab, nil
	}

	if obj, err := macho.NewFile(f); err == nil {
		if sect := obj.Section("__text"); sect != nil {
			textStart = sect.Addr
		}
		if sect := obj.Section("__gosymtab"); sect != nil {
			if symtab, err = sect.Data(); err != nil {
				return 0, nil, nil, err
			}
		}
		if sect := obj.Section("__gopclntab"); sect != nil {
			if pclntab, err = sect.Data(); err != nil {
				return 0, nil, nil, err
			}
		}
		return textStart, symtab, pclntab, nil
	}

	if obj, err := pe.NewFile(f); err == nil {
		if sect := obj.Section(".text"); sect != nil {
			textStart = uint64(sect.VirtualAddress)
		}
		if sect := obj.Section(".gosymtab"); sect != nil {
			if symtab, err = sect.Data(); err != nil {
				return 0, nil, nil, err
			}
		}
		if sect := obj.Section(".gopclntab"); sect != nil {
			if pclntab, err = sect.Data(); err != nil {
				return 0, nil, nil, err
			}
		}
		return textStart, symtab, pclntab, nil
	}

	return 0, nil, nil, fmt.Errorf("unrecognized binary format")
}

type file struct {
	mode  string
	index int
	f     program.File
}

func (s *Server) Open(req *proxyrpc.OpenRequest, resp *proxyrpc.OpenResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// TODO: Better simulation. For now we just open the named OS file.
	var flag int
	switch req.Mode {
	case "r":
		flag = os.O_RDONLY
	case "w":
		flag = os.O_WRONLY
	case "rw":
		flag = os.O_RDWR
	default:
		return fmt.Errorf("Open: bad open mode %q", req.Mode)
	}
	osFile, err := os.OpenFile(req.Name, flag, 0)
	if err != nil {
		return err
	}
	// Find a file descriptor (index) slot.
	index := 0
	for ; index < len(s.files) && s.files[index] != nil; index++ {
	}
	f := &file{
		mode:  req.Mode,
		index: index,
		f:     osFile,
	}
	if index == len(s.files) {
		s.files = append(s.files, f)
	} else {
		s.files[index] = f
	}
	return nil
}

func (s *Server) ReadAt(req *proxyrpc.ReadAtRequest, resp *proxyrpc.ReadAtResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fd := req.FD
	if fd < 0 || len(s.files) <= fd || s.files[fd] == nil {
		return fmt.Errorf("ReadAt: bad file descriptor %d", fd)
	}
	f := s.files[fd]
	buf := make([]byte, req.Len) // TODO: Don't allocate every time
	n, err := f.f.ReadAt(buf, req.Offset)
	resp.Data = buf[:n]
	return err
}

func (s *Server) Close(req *proxyrpc.CloseRequest, resp *proxyrpc.CloseResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fd := req.FD
	if fd < 0 || fd >= len(s.files) || s.files[fd] == nil {
		return fmt.Errorf("Close: bad file descriptor %d", fd)
	}
	err := s.files[fd].f.Close()
	// Remove it regardless
	s.files[fd] = nil
	return err
}

func (s *Server) Run(req *proxyrpc.RunRequest, resp *proxyrpc.RunResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.proc != nil {
		s.proc.Kill()
		s.proc = nil
	}
	p, err := s.startProcess(s.executable, nil, &os.ProcAttr{
		Files: []*os.File{
			nil,       // TODO: be able to feed the target's stdin.
			os.Stderr, // TODO: be able to capture the target's stdout.
			os.Stderr,
		},
		Sys: &syscall.SysProcAttr{
			Ptrace: !req.Start,
		},
	})
	if err != nil {
		return err
	}
	s.proc = p

	if !req.Start {
		// TODO: wait until /proc/{s.proc.Pid}/status says "State:	t (tracing stop)".
	}
	return nil
}

func (s *Server) Resume(req *proxyrpc.ResumeRequest, resp *proxyrpc.ResumeResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.ptraceCont(s.proc.Pid, 0)
	if err != nil {
		return err
	}

	err = s.wait()
	if err != nil {
		return err
	}

	regs := syscall.PtraceRegs{}
	err = s.ptraceGetRegs(s.proc.Pid, &regs)
	if err != nil {
		return err
	}

	resp.Status.PC = regs.Rip
	resp.Status.SP = regs.Rsp

	// If we're stopped on a breakpoint, restore the original code,
	// back up the PC, step through a single instruction, and reset the breakpoint.
	// TODO: should this happen here or just before the ptraceCont call?
	bp, ok := s.breakpoints[regs.Rip-1] // TODO: -1 because on amd64, INT 3 is 1 byte (0xcc).
	if ok {
		pc := uintptr(regs.Rip - 1)
		err := s.ptracePoke(s.proc.Pid, pc, []byte{bp.origInstr})
		if err != nil {
			return fmt.Errorf("ptracePoke: %v", err)
		}

		regs.Rip-- // TODO: depends on length of trap.
		err = s.ptraceSetRegs(s.proc.Pid, &regs)
		if err != nil {
			return fmt.Errorf("ptraceSetRegs: %v", err)
		}

		err = s.ptraceSingleStep(s.proc.Pid)
		if err != nil {
			return fmt.Errorf("ptraceSingleStep: %v", err)
		}

		buf := make([]byte, 1)
		buf[0] = 0xcc // INT 3 instruction on x86.
		err = s.ptracePoke(s.proc.Pid, pc, buf)
		if err != nil {
			return fmt.Errorf("ptracePoke: %v", err)
		}
	}

	return nil
}

func (s *Server) Breakpoint(req *proxyrpc.BreakpointRequest, resp *proxyrpc.BreakpointResponse) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	addrs, err := s.eval(req.Address)
	if err != nil {
		return err
	}
	for _, addr := range addrs {
		pc, err := s.evalAddress(addr)
		if err != nil {
			return err
		}
		if _, alreadySet := s.breakpoints[pc]; alreadySet {
			return fmt.Errorf("breakpoint already set at %#x (TODO)", pc)
		}

		var buf [1]byte
		err = s.ptracePeek(s.proc.Pid, uintptr(pc), buf[:])
		if err != nil {
			return fmt.Errorf("ptracePoke: %v", err)
		}

		s.breakpoints[pc] = breakpoint{pc: pc, origInstr: buf[0]}

		buf[0] = 0xcc // INT 3 instruction on x86.
		err = s.ptracePoke(s.proc.Pid, uintptr(pc), buf[:])
		if err != nil {
			return fmt.Errorf("ptracePoke: %v", err)
		}
	}

	return nil
}

func (s *Server) Eval(req *proxyrpc.EvalRequest, resp *proxyrpc.EvalResponse) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	resp.Result, err = s.eval(req.Expr)
	return err
}

// eval evaluates an expression.
// TODO: very weak.
func (s *Server) eval(expr string) ([]string, error) {
	switch {
	case strings.HasPrefix(expr, "re:"):
		// Regular expression. Return list of symbols.
		expr = expr[3:]
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, err
		}
		strs := make([]string, 0, 100)
		for _, f := range s.symbols.Funcs {
			if re.MatchString(f.Sym.Name) {
				strs = append(strs, f.Sym.Name)
			}
		}
		return strs, nil

	case strings.HasPrefix(expr, "sym:"):
		// Symbol lookup. Return address.
		expr = expr[4:]
		sym := s.symbols.LookupFunc(expr)
		if sym == nil {
			return nil, fmt.Errorf("symbol %q not found", expr)
		}
		return []string{fmt.Sprintf("%#x", sym.Value)}, nil

	case len(expr) > 0 && '0' <= expr[0] && expr[0] <= '9':
		// Numerical address. Return symbol.
		addr, err := strconv.ParseUint(expr, 0, 0)
		if err != nil {
			return nil, err
		}
		fun := s.symbols.PCToFunc(addr)
		if fun == nil {
			return nil, fmt.Errorf("address %q has no func", expr)
		}
		return []string{fun.Sym.Name}, nil
	}

	return nil, fmt.Errorf("bad expression syntax: %q", expr)
}

// evalAddress takes a simple expression, either a symbol or hex value,
// and evaluates it as an address.
func (s *Server) evalAddress(expr string) (uint64, error) {
	// Might be a symbol.
	sym := s.symbols.LookupFunc(expr)
	if sym != nil {
		return sym.Value, nil
	}

	// Must be a number.
	addr, err := strconv.ParseUint(expr, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("eval: %q is neither symbol nor number", expr)
	}

	return addr, nil

}