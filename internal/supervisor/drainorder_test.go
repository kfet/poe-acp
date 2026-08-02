//go:build unix

package supervisor

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---- kinds and orders ----

func TestDrainKind(t *testing.T) {
	for _, tc := range []struct {
		kind     DrainKind
		str      string
		flag     string
		deadline time.Duration
	}{
		{DrainStop, "stop", "-drain-deadline", DefaultDrainDeadline},
		{DrainSwap, "swap", "-swap-drain-deadline", DefaultSwapDrainDeadline},
	} {
		if got := tc.kind.String(); got != tc.str {
			t.Fatalf("String() = %q want %q", got, tc.str)
		}
		if got := tc.kind.Flag(); got != tc.flag {
			t.Fatalf("Flag() = %q want %q", got, tc.flag)
		}
		if got := tc.kind.DefaultDeadline(); got != tc.deadline {
			t.Fatalf("DefaultDeadline() = %s want %s", got, tc.deadline)
		}
	}
}

// TestDefaultSwapDrainDeadline_IsNotTheStopDeadline pins the whole point
// of the split: a reload must not be bounded by the stop budget, which is
// what force-closed a healthy 42-minute turn.
func TestDefaultSwapDrainDeadline_IsNotTheStopDeadline(t *testing.T) {
	if DefaultSwapDrainDeadline <= DefaultDrainDeadline {
		t.Fatalf("swap deadline %s must be far longer than the stop deadline %s",
			DefaultSwapDrainDeadline, DefaultDrainDeadline)
	}
	if DefaultDrainDeadline+RetireGrace >= 90*time.Second {
		t.Fatalf("stop chain %s must stay under systemd TimeoutStopSec=90s", DefaultDrainDeadline+RetireGrace)
	}
}

// TestDrainOrder_NormalizePerKind: an unset deadline falls back to the
// default for THAT kind, so a swap never silently inherits the stop bound.
func TestDrainOrder_NormalizePerKind(t *testing.T) {
	if got := (DrainOrder{Kind: DrainStop}).Normalize().Deadline; got != DefaultDrainDeadline {
		t.Fatalf("stop default = %s want %s", got, DefaultDrainDeadline)
	}
	if got := (DrainOrder{Kind: DrainSwap}).Normalize().Deadline; got != DefaultSwapDrainDeadline {
		t.Fatalf("swap default = %s want %s", got, DefaultSwapDrainDeadline)
	}
	if got := (DrainOrder{Kind: DrainSwap, Deadline: time.Minute}).Normalize().Deadline; got != time.Minute {
		t.Fatalf("explicit deadline must survive normalization, got %s", got)
	}
}

func TestDrainOrder_RetireDeadlineAndString(t *testing.T) {
	o := DrainOrder{Kind: DrainSwap, Deadline: time.Minute}
	if got, want := o.RetireDeadline(), time.Minute+RetireGrace; got != want {
		t.Fatalf("RetireDeadline() = %s want %s", got, want)
	}
	// The escalation bound must track the SWAP deadline, not the stop one.
	if got, want := (DrainOrder{Kind: DrainSwap}).RetireDeadline(), DefaultSwapDrainDeadline+RetireGrace; got != want {
		t.Fatalf("swap RetireDeadline() = %s want %s", got, want)
	}
	if got := o.String(); got != "swap drain (deadline 1m0s)" {
		t.Fatalf("String() = %q", got)
	}
}

// ---- wire format ----

func TestDrainOrder_WireRoundTrip(t *testing.T) {
	for _, want := range []DrainOrder{
		{Kind: DrainSwap, Deadline: 30 * time.Minute},
		{Kind: DrainStop, Deadline: 45 * time.Second},
		{Kind: DrainSwap}, // normalized on the way out
	} {
		got, err := parseDrainOrder(want.encode())
		if err != nil {
			t.Fatalf("parse(%q): %v", want.encode(), err)
		}
		if got != want.Normalize() {
			t.Fatalf("round trip = %+v want %+v", got, want.Normalize())
		}
	}
}

func TestDrainOrder_EncodeFitsOneRead(t *testing.T) {
	line := DrainOrder{Kind: DrainSwap, Deadline: 30 * time.Minute}.encode()
	if len(line) > maxDrainOrder {
		t.Fatalf("order line %d bytes exceeds the worker's %d-byte read", len(line), maxDrainOrder)
	}
	if !strings.HasSuffix(string(line), "\n") {
		t.Fatalf("order line must be newline terminated: %q", line)
	}
}

func TestParseDrainOrder_Rejects(t *testing.T) {
	for _, bad := range []string{
		"",
		"drain kind=swap",
		"nonsense kind=swap deadline=1m",
		"drain kind=explode deadline=1m",
		"drain kind=swap 30m",
		"drain kind=swap deadline=banana",
		"drain kind=stop deadline=0s",
		"drain kind=stop deadline=-5s",
	} {
		if o, err := parseDrainOrder([]byte(bad)); err == nil {
			t.Fatalf("parse(%q) accepted %+v, want error", bad, o)
		}
	}
}

// ---- worker-side Control ----

// newTestControl wires a real pipe to a Control, returning the write end
// the "supervisor" uses.
func newTestControl(t *testing.T) (*Control, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	// Fd() hands over a blocking, poller-detached fd; Control takes
	// ownership of it for the rest of the test.
	c := newControl(int(r.Fd()))
	t.Cleanup(func() { closeFile(r); closeFile(w) })
	return c, w
}

// TestControl_PendingOrderWins is the swap half of the fix: an order
// written before the SIGTERM is what bounds the drain, NOT the worker's
// own stop deadline.
func TestControl_PendingOrderWins(t *testing.T) {
	c, w := newTestControl(t)
	if _, err := w.Write(DrainOrder{Kind: DrainSwap, Deadline: 30 * time.Minute}.encode()); err != nil {
		t.Fatalf("write order: %v", err)
	}
	got, err := c.DrainOrder(45 * time.Second)
	if err != nil {
		t.Fatalf("DrainOrder: %v", err)
	}
	if got.Kind != DrainSwap || got.Deadline != 30*time.Minute {
		t.Fatalf("got %+v want a 30m swap drain", got)
	}
}

// TestControl_NoOrderIsAStopDrain is the stop half: systemd SIGTERMs the
// cgroup and writes nothing, so the worker must fall back to its own
// -drain-deadline without blocking.
func TestControl_NoOrderIsAStopDrain(t *testing.T) {
	c, _ := newTestControl(t)
	got, err := c.DrainOrder(45 * time.Second)
	if err != nil {
		t.Fatalf("DrainOrder: %v", err)
	}
	if got.Kind != DrainStop || got.Deadline != 45*time.Second {
		t.Fatalf("got %+v want a 45s stop drain", got)
	}
}

// TestControl_ClosedPipeIsAStopDrain: the supervisor died without ever
// sending an order (read returns EOF).
func TestControl_ClosedPipeIsAStopDrain(t *testing.T) {
	c, w := newTestControl(t)
	closeFile(w)
	got, err := c.DrainOrder(time.Minute)
	if err != nil || got.Kind != DrainStop || got.Deadline != time.Minute {
		t.Fatalf("got (%+v,%v) want a 1m stop drain", got, err)
	}
}

func TestControl_MalformedOrderFallsBackToStop(t *testing.T) {
	c, w := newTestControl(t)
	if _, err := w.Write([]byte("drain kind=zzz deadline=1m\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := c.DrainOrder(time.Minute)
	if err == nil {
		t.Fatal("want a parse error surfaced for logging")
	}
	if got.Kind != DrainStop || got.Deadline != time.Minute {
		t.Fatalf("got %+v want the safe stop fallback", got)
	}
}

func TestControl_ReadErrorFallsBackToStop(t *testing.T) {
	c, _ := newTestControl(t)
	// Close the fd out from under Control: the next read is EBADF.
	if err := syscall.Close(c.fd); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, err := c.DrainOrder(time.Minute)
	if err == nil {
		t.Fatal("want the read error surfaced for logging")
	}
	if got.Kind != DrainStop || got.Deadline != time.Minute {
		t.Fatalf("got %+v want the safe stop fallback", got)
	}
	c.fd = -1 // the cleanup close would otherwise hit a stale fd
}

// TestControl_RetriesEINTR: a signal landing mid-read must not be
// mistaken for "no order pending" — that would silently downgrade a swap
// drain to the 45s stop bound.
func TestControl_RetriesEINTR(t *testing.T) {
	c, w := newTestControl(t)
	if _, err := w.Write(DrainOrder{Kind: DrainSwap, Deadline: time.Hour}.encode()); err != nil {
		t.Fatalf("write order: %v", err)
	}
	old := readFD
	first := true
	readFD = func(fd int, p []byte) (int, error) {
		if first {
			first = false
			return 0, syscall.EINTR
		}
		return old(fd, p)
	}
	t.Cleanup(func() { readFD = old })

	got, err := c.DrainOrder(45 * time.Second)
	if err != nil {
		t.Fatalf("DrainOrder: %v", err)
	}
	if got.Kind != DrainSwap || got.Deadline != time.Hour {
		t.Fatalf("got %+v want the 1h swap order after the EINTR retry", got)
	}
}

// TestWorkerControl_UnsetOrUnusable: a worker with no control pipe — one
// started outside a supervisor, or handed a broken fd — reports stop
// semantics rather than failing or blocking.
func TestWorkerControl_UnsetOrUnusable(t *testing.T) {
	os.Unsetenv(EnvControlFD)
	for _, c := range []*Control{
		WorkerControl(),
		newControl(-1),
		newControl(999999), // not an open fd: SetNonblock fails
	} {
		if c.fd != -1 {
			t.Fatalf("want a disabled control, got fd %d", c.fd)
		}
		got, err := c.DrainOrder(time.Minute)
		if err != nil || got.Kind != DrainStop || got.Deadline != time.Minute {
			t.Fatalf("got (%+v,%v) want a 1m stop drain", got, err)
		}
	}
}

func TestWorkerControl_FromEnv(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer closeFile(r)
	defer closeFile(w)
	if _, err := w.Write(DrainOrder{Kind: DrainSwap, Deadline: 20 * time.Minute}.encode()); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv(EnvControlFD, strconv.Itoa(int(r.Fd())))
	got, err := WorkerControl().DrainOrder(time.Minute)
	if err != nil {
		t.Fatalf("DrainOrder: %v", err)
	}
	if got.Kind != DrainSwap || got.Deadline != 20*time.Minute {
		t.Fatalf("got %+v want the 20m swap order", got)
	}
}

// ---- supervisor -> worker, end to end ----

// TestRetire_HandsWorkerTheDrainOrder is the whole mechanism in one test:
// the supervisor writes the order for THIS retirement to the worker's
// control pipe before the SIGTERM, and the worker reads back exactly the
// deadline the supervisor chose — a swap drain gets the swap deadline, a
// stop drain gets the stop deadline.
func TestRetire_HandsWorkerTheDrainOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order DrainOrder
	}{
		{"swap", DrainOrder{Kind: DrainSwap, Deadline: 30 * time.Minute}},
		{"stop", DrainOrder{Kind: DrainStop, Deadline: 45 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSup(t)
			rec := stubSignals(t, nil)

			// Stand in for the forked worker: the supervisor's write end
			// in s.control, the worker's Control on the read end.
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			defer closeFile(r)
			const pid = 4242
			s.mu.Lock()
			s.control = map[int]*os.File{pid: w}
			s.mu.Unlock()
			worker := newControl(int(r.Fd()))

			dead := make(chan struct{})
			close(dead)
			if res, err := s.Retire(&os.Process{Pid: pid}, dead, tc.order, nil); err != nil || res != RetireGraceful {
				t.Fatalf("Retire = (%v,%v) want (graceful,nil)", res, err)
			}
			if sent := rec.list(); len(sent) != 1 || sent[0] != syscall.SIGTERM {
				t.Fatalf("want a single SIGTERM, got %v", sent)
			}

			got, err := worker.DrainOrder(DefaultDrainDeadline)
			if err != nil {
				t.Fatalf("worker DrainOrder: %v", err)
			}
			if got != tc.order {
				t.Fatalf("worker read %+v want %+v", got, tc.order)
			}
		})
	}
}

// TestRetire_LostOrderStillRetiresOnTheStopBound: if the order cannot be
// delivered the retirement proceeds anyway — the worker falls back to its
// own (shorter) stop deadline, which is safe.
func TestRetire_LostOrderStillRetiresOnTheStopBound(t *testing.T) {
	s := newSup(t)
	stubSignals(t, nil)
	dead := make(chan struct{})
	close(dead)
	res, err := s.Retire(&os.Process{Pid: 4242}, dead, DrainOrder{Kind: DrainSwap}, nil)
	if err != nil || res != RetireGraceful {
		t.Fatalf("got (%v,%v) want (graceful,nil)", res, err)
	}
}

// TestSendDrainOrder_WriteError covers a control pipe whose reader is
// gone: the write fails with EPIPE rather than killing the supervisor.
func TestSendDrainOrder_WriteError(t *testing.T) {
	s := newSup(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	closeFile(r) // no reader: the write end is broken
	s.mu.Lock()
	s.control = map[int]*os.File{7: w}
	s.mu.Unlock()
	if err := s.sendDrainOrder(7, DrainOrder{Kind: DrainSwap}); err == nil {
		t.Fatal("want a write error for a pipe with no reader")
	} else if !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("want EPIPE, got %v", err)
	}
}

// TestReleaseWorker closes the control pipe exactly once and tolerates an
// unknown pid (a double reap).
func TestReleaseWorker(t *testing.T) {
	s := newSup(t)
	_, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	s.mu.Lock()
	s.control = map[int]*os.File{9: w}
	s.mu.Unlock()

	s.ReleaseWorker(9)
	s.ReleaseWorker(9) // unknown now: must not panic or double-close
	if err := s.sendDrainOrder(9, DrainOrder{}); err == nil {
		t.Fatal("a released worker must have no control pipe")
	}
}

// TestSpawn_RegistersControlPipe: every spawned worker gets a control
// pipe, the child is told where to find it, and a drain order written
// afterwards reaches the fd the child inherited.
func TestSpawn_RegistersControlPipe(t *testing.T) {
	s := newSup(t)
	var ctlR *os.File
	var childEnv []string
	s.start = func(c *exec.Cmd) error {
		if len(c.ExtraFiles) != 3 {
			t.Fatalf("want listener+death+control in ExtraFiles, got %d", len(c.ExtraFiles))
		}
		// Stand in for the fork: take our own copy of the read end, as
		// the child would, so Spawn closing the parent's copy is fine.
		fd, err := syscall.Dup(int(c.ExtraFiles[2].Fd()))
		if err != nil {
			t.Fatalf("dup: %v", err)
		}
		ctlR = os.NewFile(uintptr(fd), "control")
		childEnv = c.Env
		c.Process = &os.Process{Pid: 3131}
		return nil
	}
	p, err := s.Spawn()
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer closeFile(ctlR)
	want := EnvControlFD + "=" + strconv.Itoa(controlChildFD)
	if !slices.Contains(childEnv, want) {
		t.Fatalf("worker env missing %q: %v", want, childEnv)
	}

	if err := s.sendDrainOrder(p.Pid, DrainOrder{Kind: DrainSwap, Deadline: time.Hour}); err != nil {
		t.Fatalf("sendDrainOrder: %v", err)
	}
	buf := make([]byte, maxDrainOrder)
	n, err := ctlR.Read(buf)
	if err != nil {
		t.Fatalf("read inherited control fd: %v", err)
	}
	got, err := parseDrainOrder(buf[:n])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Kind != DrainSwap || got.Deadline != time.Hour {
		t.Fatalf("child received %+v want a 1h swap order", got)
	}
}

func TestSpawn_ControlPipeError(t *testing.T) {
	s := newSup(t)
	old := pipeFn
	pipeFn = func() (*os.File, *os.File, error) { return nil, nil, errors.New("boom") }
	t.Cleanup(func() { pipeFn = old })
	if _, err := s.Spawn(); err == nil {
		t.Fatal("want a control-pipe allocation error")
	}
}
