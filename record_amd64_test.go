// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package perf_test

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"acln.ro/perf"
)

func TestSampleUserRegisters(t *testing.T) {
	requires(t, tracepointPMU, debugfs) // TODO(acln): paranoid

	// TODO(acln): rewrite this to use groups rather than manual wiring,
	// when reading sampling records from groups is supported correctly.

	wea := &perf.Attr{
		CountFormat: perf.CountFormat{
			Group: true,
		},
		SampleFormat: perf.SampleFormat{
			UserRegisters: true,
		},
		Options: perf.Options{
			SampleIDAll: true,
		},
		// RDI, RSI, RDX. See arch/x86/include/uapi/asm/perf_regs.h.
		SampleRegistersUser: 0x38,
	}
	wea.SetSamplePeriod(1)
	wea.SetWakeupEvents(1)
	wetp := perf.Tracepoint("syscalls", "sys_enter_write")
	if err := wetp.Configure(wea); err != nil {
		t.Fatal(err)
	}

	wxa := &perf.Attr{
		SampleFormat: perf.SampleFormat{
			UserRegisters: true,
		},
		Options: perf.Options{
			SampleIDAll: true,
		},
		// RAX. See arch/x86/include/uapi/asm/perf_regs.h.
		SampleRegistersUser: 0x1,
	}
	wxa.SetSamplePeriod(1)
	wxa.SetWakeupEvents(1)
	wxtp := perf.Tracepoint("syscalls", "sys_exit_write")
	if err := wxtp.Configure(wxa); err != nil {
		t.Fatal(err)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	wentry, err := perf.Open(wea, perf.CallingThread, perf.AnyCPU, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer wentry.Close()
	if err := wentry.MapRing(); err != nil {
		t.Fatal(err)
	}

	wexit, err := perf.Open(wxa, perf.CallingThread, perf.AnyCPU, wentry)
	if err != nil {
		t.Fatal(err)
	}
	defer wexit.Close()
	if err := wexit.MapRing(); err != nil {
		t.Fatal(err)
	}

	null, err := os.OpenFile("/dev/null", os.O_WRONLY, 0200)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()

	buf := make([]byte, 8)

	var n int
	var werr error
	gc, err := wentry.MeasureGroup(func() {
		n, werr = null.Write(buf)
	})
	if err != nil {
		t.Fatal(err)
	}
	if werr != nil {
		t.Fatal(err)
	}
	if entry := gc.Values[0].Value; entry != 1 {
		t.Fatalf("got %d hits for write at entry, want 1", entry)
	}
	if exit := gc.Values[1].Value; exit != 1 {
		t.Fatalf("got %d hits for write at exit, want 1", exit)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	entryrec, err := wentry.ReadRecord(ctx)
	if err != nil {
		t.Fatalf("got %v, want a valid record", err)
	}
	entrysr, ok := entryrec.(*perf.SampleGroupRecord)
	if !ok {
		t.Fatalf("got %T, want SampleGroupRecord", entryrec)
	}
	if nregs := len(entrysr.UserRegisters); nregs != 3 {
		t.Fatalf("got %d registers, want 3", nregs)
	}

	var (
		rdi = entrysr.UserRegisters[2]
		rsi = entrysr.UserRegisters[1]
		rdx = entrysr.UserRegisters[0]

		nullfd  = uint64(null.Fd())
		bufp    = uint64(uintptr(unsafe.Pointer(&buf[0])))
		bufsize = uint64(len(buf))
	)

	if rdi != nullfd {
		t.Errorf("fd: rdi = %d, want %d", rdi, nullfd)
	}
	if rsi != bufp {
		t.Errorf("buf: rsi = %#x, want %#x", rsi, bufp)
	}
	if rdx != bufsize {
		t.Errorf("count: rdx = %d, want %d", rdx, bufsize)
	}

	exitrec, err := wexit.ReadRecord(ctx)
	if err != nil {
		t.Fatalf("got %v, want a valid record", err)
	}
	exitsr, ok := exitrec.(*perf.SampleRecord)
	if !ok {
		t.Fatalf("got %T, want SampleRecord", exitrec)
	}
	if nregs := len(exitsr.UserRegisters); nregs != 1 {
		t.Fatalf("got %d registers, want 1", nregs)
	}

	rax := exitsr.UserRegisters[0]
	if uint64(n) != rax {
		t.Fatalf("return: rax = %d, want %d", n, rax)
	}
}
