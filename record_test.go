// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package perf_test

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"acln.ro/perf"

	"golang.org/x/sys/unix"
)

// TODO(acln): the paranoid requirement is not specified for these tests

func TestRecord(t *testing.T) {
	t.Run("RingPoll", testRingPoll)
	t.Run("SampleTracepointPid", testSampleTracepointPid)
	t.Run("SampleTracepointPidConcurrent", testSampleTracepointPidConcurrent)
	t.Run("SampleTracepointStack", testSampleTracepointStack)
	t.Run("RedirectManualWire", testRedirectManualWire)
}

func testRingPoll(t *testing.T) {
	t.Run("Timeout", testPollTimeout)
	t.Run("Cancel", testPollCancel)
	t.Run("Expired", testPollExpired)
	t.Run("DisabledExit", testPollDisabledProcessExit)
	t.Run("DisabledRefresh", testPollDisabledRefresh)
}

func testPollTimeout(t *testing.T) {
	requires(t, tracepointPMU, debugfs)

	ga := new(perf.Attr)
	ga.SetSamplePeriod(1)
	ga.SetWakeupEvents(1)
	gtp := perf.Tracepoint("syscalls", "sys_enter_getpid")
	if err := gtp.Configure(ga); err != nil {
		t.Fatal(err)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	getpid, err := perf.Open(ga, perf.CallingThread, perf.AnyCPU, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer getpid.Close()
	if err := getpid.MapRing(); err != nil {
		t.Fatal(err)
	}

	errch := make(chan error)
	timeout := 20 * time.Millisecond

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		for i := 0; i < 2; i++ {
			_, err := getpid.ReadRecord(ctx)
			errch <- err
		}
	}()

	c, err := getpid.Measure(getpidTrigger)
	if err != nil {
		t.Fatal(err)
	}
	if c.Value != 1 {
		t.Fatalf("got %d hits for %q, want 1", c.Value, c.Label)
	}

	// For the first event, we should get a valid sample immediately.
	select {
	case <-time.After(10 * time.Millisecond):
		t.Fatalf("didn't get the first sample: timeout")
	case err := <-errch:
		if err != nil {
			t.Fatalf("got %v, want valid first sample", err)
		}
	}

	// Now, we should get a timeout.
	select {
	case <-time.After(2 * timeout):
		t.Logf("didn't time out, waiting")
		err := <-errch
		t.Fatalf("got %v", err)
	case err := <-errch:
		if err != context.DeadlineExceeded {
			t.Fatalf("got %v, want context.DeadlineExceeded", err)
		}
	}
}

func testPollCancel(t *testing.T) {
	requires(t, tracepointPMU, debugfs)

	ga := new(perf.Attr)
	ga.SetSamplePeriod(1)
	ga.SetWakeupEvents(1)
	gtp := perf.Tracepoint("syscalls", "sys_enter_getpid")
	if err := gtp.Configure(ga); err != nil {
		t.Fatal(err)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	getpid, err := perf.Open(ga, perf.CallingThread, perf.AnyCPU, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer getpid.Close()
	if err := getpid.MapRing(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errch := make(chan error)

	go func() {
		for i := 0; i < 2; i++ {
			_, err := getpid.ReadRecord(ctx)
			errch <- err
		}
	}()

	c, err := getpid.Measure(getpidTrigger)
	if err != nil {
		t.Fatal(err)
	}
	if c.Value != 1 {
		t.Fatalf("got %d hits for %q, want 1", c.Value, c.Label)
	}

	// For the first event, we should get a valid sample.
	select {
	case <-time.After(10 * time.Millisecond):
		t.Fatalf("didn't get the first sample: timeout")
	case err := <-errch:
		if err != nil {
			t.Fatalf("got %v, want valid first sample", err)
		}
	}

	// The goroutine reading the records is now blocked in ReadRecord.
	// Cancel the context and observe the results. We should see
	// context.Canceled quite quickly.
	cancel()

	select {
	case <-time.After(10 * time.Millisecond):
		t.Fatalf("context cancel didn't unblock ReadRecord")
	case err := <-errch:
		if err != context.Canceled {
			t.Fatalf("got %v, want %v", err, context.Canceled)
		}
	}
}

func testPollExpired(t *testing.T) {
	requires(t, softwarePMU)

	da := new(perf.Attr)
	perf.Dummy.Configure(da)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	dummy, err := perf.Open(da, perf.CallingThread, perf.AnyCPU, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dummy.Close()
	if err := dummy.MapRing(); err != nil {
		t.Fatal(err)
	}

	timeout := 1 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Wait until the deadline is in the past.
	time.Sleep(2 * timeout)

	rec, err := dummy.ReadRecord(ctx)
	if err == nil {
		t.Fatalf("got nil error and record %#v", rec)
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
}

const errDisabledTestEnv = "PERF_TEST_ERR_DISABLED"

func init() {
	// In child process of testErrDisabledProcessExist.
	errDisabledTest := os.Getenv(errDisabledTestEnv)
	if errDisabledTest != "1" {
		return
	}

	// Signal to the parent that we can start.
	evsig(3)

	// Wait for the parent to tell us that they have set up performance
	// monitoring, and are ready to observe the event.
	evwait(4)

	// Call getpid, then exit. Parent will see POLLIN for getpid, then
	// POLLHUP because we exited.
	unix.Getpid()
	os.Exit(0)
}

func testPollDisabledProcessExit(t *testing.T) {
	requires(t, tracepointPMU, debugfs)

	// Re-exec ourselves with PERF_TEST_ERR_DISABLED=1.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	readyev, err := unix.Eventfd(0, 0)
	if err != nil {
		t.Fatal(err)
	}

	startev, err := unix.Eventfd(0, 0)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), errDisabledTestEnv+"=1")
	cmd.ExtraFiles = []*os.File{
		os.NewFile(uintptr(readyev), "readyevfd"),
		os.NewFile(uintptr(startev), "startevfd"),
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Set up performance monitoring for the child process.
	ga := &perf.Attr{
		Options: perf.Options{
			Disabled: true,
		},
		SampleFormat: perf.SampleFormat{
			Tid: true,
		},
	}
	ga.SetSamplePeriod(1)
	ga.SetWakeupEvents(1)
	gtp := perf.Tracepoint("syscalls", "sys_enter_getpid")
	if err := gtp.Configure(ga); err != nil {
		t.Fatal(err)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	getpid, err := perf.Open(ga, cmd.Process.Pid, perf.AnyCPU, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer getpid.Close()
	if err := getpid.MapRing(); err != nil {
		t.Fatal(err)
	}

	// Wait for the child process to be ready.
	evwait(readyev)

	// Now that it is, enable the event.
	if err := getpid.Enable(); err != nil {
		t.Fatal(err)
	}

	var rec1, rec2 perf.Record
	var err1, err2 error
	done := make(chan struct{})

	go func() {
		timeout := 100 * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		// Read two records. The first one should be valid,
		// the second one should not, and the second error
		// should be ErrDisabled.
		rec1, err1 = getpid.ReadRecord(ctx)
		rec2, err2 = getpid.ReadRecord(ctx)
		close(done)
	}()

	// Signal to the child that it should call getpid now.
	evsig(startev)

	<-done
	if err1 != nil {
		t.Errorf("first error was %v, want nil", err1)
	}
	sr, ok := rec1.(*perf.SampleRecord)
	if !ok {
		t.Errorf("first record: got %T, want a SampleRecord", rec1)
	}
	if int(sr.Pid) != cmd.Process.Pid {
		t.Errorf("first record: got pid %d in the sample, want %d",
			sr.Pid, cmd.Process.Pid)
	}
	if err2 != perf.ErrDisabled {
		t.Errorf("second record: error was %v, want ErrDisabled", err2)
	}
	if rec2 != nil {
		t.Errorf("second record: got %#v, want nil", rec2)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("wait: %v", err)
	}
}

func evsig(fd int) {
	val := uint64(1)
	buf := (*[8]byte)(unsafe.Pointer(&val))[:]
	unix.Write(fd, buf)
}

func evwait(fd int) {
	var val uint64
	buf := (*[8]byte)(unsafe.Pointer(&val))[:]
	unix.Read(fd, buf)
}

func testPollDisabledRefresh(t *testing.T) {
	t.Skip("TODO(acln): investigate POLLHUP and IOC_REFRESH")

	requires(t, tracepointPMU, debugfs)

	ga := &perf.Attr{
		SampleFormat: perf.SampleFormat{
			Tid: true,
		},
		Options: perf.Options{
			Disabled: true,
		},
	}
	ga.SetSamplePeriod(1)
	ga.SetWakeupEvents(1)
	gtp := perf.Tracepoint("syscalls", "sys_enter_getpid")
	if err := gtp.Configure(ga); err != nil {
		t.Fatal(err)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	getpid, err := perf.Open(ga, perf.CallingThread, perf.AnyCPU, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer getpid.Close()
	if err := getpid.MapRing(); err != nil {
		t.Fatal(err)
	}

	const n = 5

	var (
		records []perf.Record
		errs    []error
	)

	done := make(chan struct{})

	if err := getpid.Enable(); err != nil {
		t.Fatal(err)
	}

	go func() {
		for i := 0; i < n+1; i++ {
			rec, err := getpid.ReadRecord(context.Background())
			records = append(records, rec)
			errs = append(errs, err)
		}
		close(done)
	}()

	if err := getpid.Refresh(4); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < n; i++ {
		getpidTrigger()
	}

	<-done

	t.Log(errs)
}

func testSampleTracepointPid(t *testing.T) {
	requires(t, tracepointPMU, debugfs)

	ga := &perf.Attr{
		SampleFormat: perf.SampleFormat{
			Tid: true,
		},
	}
	ga.SetSamplePeriod(1)
	ga.SetWakeupEvents(1)
	gtp := perf.Tracepoint("syscalls", "sys_enter_getpid")
	if err := gtp.Configure(ga); err != nil {
		t.Fatal(err)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	getpid, err := perf.Open(ga, perf.CallingThread, perf.AnyCPU, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer getpid.Close()
	if err := getpid.MapRing(); err != nil {
		t.Fatal(err)
	}

	c, err := getpid.Measure(getpidTrigger)
	if err != nil {
		t.Fatal(err)
	}
	if c.Value != 1 {
		t.Fatalf("got %d hits for %q, want 1 hit", c.Value, c.Label)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	rec, err := getpid.ReadRecord(ctx)
	if err != nil {
		t.Fatalf("got %v, want a valid sample record", err)
	}
	sr, ok := rec.(*perf.SampleRecord)
	if !ok {
		t.Fatalf("got a %T, want a SampleRecord", rec)
	}
	pid, tid := unix.Getpid(), unix.Gettid()
	if int(sr.Pid) != pid || int(sr.Tid) != tid {
		t.Fatalf("got pid=%d tid=%d, want pid=%d tid=%d", sr.Pid, sr.Tid, pid, tid)
	}
}

func testSampleTracepointPidConcurrent(t *testing.T) {
	requires(t, tracepointPMU, debugfs)

	ga := &perf.Attr{
		SampleFormat: perf.SampleFormat{
			Tid: true,
		},
	}
	ga.SetSamplePeriod(1)
	ga.SetWakeupEvents(1)
	gtp := perf.Tracepoint("syscalls", "sys_enter_getpid")
	if err := gtp.Configure(ga); err != nil {
		t.Fatal(err)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	getpid, err := perf.Open(ga, perf.CallingThread, perf.AnyCPU, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer getpid.Close()
	if err := getpid.MapRing(); err != nil {
		t.Fatal(err)
	}

	const n = 6
	sawSample := make(chan bool)

	go func() {
		for i := 0; i < n; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			rec, err := getpid.ReadRecord(ctx)
			_, isSample := rec.(*perf.SampleRecord)
			if err == nil && isSample {
				sawSample <- true
			} else {
				sawSample <- false
			}
		}
	}()

	seen := 0

	c, err := getpid.Measure(func() {
		for i := 0; i < n; i++ {
			getpidTrigger()
			if ok := <-sawSample; ok {
				seen++
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Value != n {
		t.Fatalf("got %d hits for %q, want %d", c.Value, c.Label, n)
	}
	if seen != n {
		t.Fatalf("saw %d samples, want %d", seen, n)
	}
}

func testSampleTracepointStack(t *testing.T) {
	requires(t, tracepointPMU, debugfs)

	ga := &perf.Attr{
		Options: perf.Options{
			Disabled: true,
		},
		SampleFormat: perf.SampleFormat{
			Tid:       true,
			Time:      true,
			CPU:       true,
			IP:        true,
			Callchain: true,
		},
	}
	ga.SetSamplePeriod(1)
	ga.SetWakeupEvents(1)
	gtp := perf.Tracepoint("syscalls", "sys_enter_getpid")
	if err := gtp.Configure(ga); err != nil {
		t.Fatal(err)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	getpid, err := perf.Open(ga, perf.CallingThread, perf.AnyCPU, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer getpid.Close()
	if err := getpid.MapRing(); err != nil {
		t.Fatal(err)
	}

	pcs := make([]uintptr, 10)
	var n int

	c, err := getpid.Measure(func() {
		n = runtime.Callers(2, pcs)
		getpidTrigger()
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Value != 1 {
		t.Fatalf("want 1 hit for %q, got %d", c.Label, c.Value)
	}

	pcs = pcs[:n]

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	rec, err := getpid.ReadRecord(ctx)
	if err != nil {
		t.Fatal(err)
	}
	getpidsample, ok := rec.(*perf.SampleRecord)
	if !ok {
		t.Fatalf("got a %T, want a *SampleRecord", rec)
	}

	i := len(pcs) - 1
	j := len(getpidsample.Callchain) - 1

	for i >= 0 && j >= 0 {
		gopc := pcs[i]
		kpc := getpidsample.Callchain[j]
		if gopc != uintptr(kpc) {
			t.Fatalf("Go (%#x) and kernel (%#x) PC differ", gopc, kpc)
		}
		i--
		j--
	}

	logFrame := func(pc uintptr) {
		fn := runtime.FuncForPC(pc)
		if fn == nil {
			t.Logf("%#x <nil>", pc)
		} else {
			file, line := fn.FileLine(pc)
			t.Logf("%#x %s:%d %s", pc, file, line, fn.Name())
		}
	}

	t.Log("kernel callchain:")
	for _, kpc := range getpidsample.Callchain {
		logFrame(uintptr(kpc))
	}

	t.Log()

	t.Logf("Go stack:")
	for _, gopc := range pcs {
		logFrame(gopc)
	}
}

func testRedirectManualWire(t *testing.T) {
	requires(t, tracepointPMU, debugfs)

	ga := &perf.Attr{
		SampleFormat: perf.SampleFormat{
			Tid:      true,
			Time:     true,
			CPU:      true,
			Addr:     true,
			StreamID: true,
		},
		CountFormat: perf.CountFormat{
			Group: true,
		},
		Options: perf.Options{
			Disabled: true,
		},
	}
	ga.SetSamplePeriod(1)
	ga.SetWakeupEvents(1)
	gtp := perf.Tracepoint("syscalls", "sys_enter_getpid")
	if err := gtp.Configure(ga); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	leader, err := perf.Open(ga, perf.CallingThread, perf.AnyCPU, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	if err := leader.MapRing(); err != nil {
		t.Fatal(err)
	}

	wa := &perf.Attr{
		SampleFormat: perf.SampleFormat{
			Tid:      true,
			Time:     true,
			CPU:      true,
			Addr:     true,
			StreamID: true,
		},
	}
	wa.SetSamplePeriod(1)
	wa.SetWakeupEvents(1)
	wtp := perf.Tracepoint("syscalls", "sys_enter_write")
	if err := wtp.Configure(wa); err != nil {
		t.Fatal(err)
	}

	follower, err := perf.Open(wa, perf.CallingThread, perf.AnyCPU, leader)
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close()
	if err := follower.SetOutput(leader); err != nil {
		t.Fatal(err)
	}

	errch := make(chan error)
	go func() {
		for i := 0; i < 2; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			_, err := leader.ReadRecord(ctx)
			errch <- err
		}
	}()

	gc, err := leader.MeasureGroup(func() {
		getpidTrigger()
		writeTrigger()
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := gc.Values[0]; got.Value != 1 {
		t.Fatalf("got %d hits for %q, want 1 hit", got.Value, got.Label)
	}
	if got := gc.Values[1]; got.Value != 1 {
		t.Fatalf("got %d hits for %q, want 1 hit", got.Value, got.Label)
	}

	for i := 0; i < 2; i++ {
		select {
		case <-time.After(10 * time.Millisecond):
			t.Errorf("did not get sample record: timeout")
		case err := <-errch:
			if err != nil {
				t.Fatalf("did not get sample record: %v", err)
			}
		}
	}
}
