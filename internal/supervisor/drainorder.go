//go:build unix

package supervisor

// Drain orders: how the supervisor tells ONE worker which retirement
// contract its SIGTERM falls under.
//
// A worker gets SIGTERM in two very different situations, and they want
// opposite bounds:
//
//   - STOP — the whole service is going down (systemd SIGTERMs the
//     cgroup and gives up at TimeoutStopSec=90s; a supervisor
//     self-upgrade is quiescent until the worker is gone). Something
//     external IS waiting, so the drain must be short:
//     DefaultDrainDeadline (45s).
//   - SWAP — a SIGHUP worker swap. The supervisor survives, the NEW
//     worker is already accepting, and nothing external is waiting: the
//     retiring worker exists only to finish the streams it still holds.
//     Time is the wrong resource to bound here — liveness is, and the
//     per-stream idle-write backstop already reaps a wedged turn. So the
//     bound is a leak backstop, not a working bound:
//     DefaultSwapDrainDeadline (30m).
//
// The worker cannot tell the two apart — SIGTERM is SIGTERM, and on a
// systemd stop it does not even come from the supervisor. The SUPERVISOR
// always knows, so it says so explicitly: it writes one DrainOrder line
// to that worker's control pipe and THEN sends SIGTERM. The write
// completes before the kill(2), so the bytes are already in the pipe
// buffer when the worker's SIGTERM handler does its single non-blocking
// read — no timer, no race, no inference.
//
// Absence of an order means STOP. That is what makes the mechanism safe
// under version skew and under a SIGTERM from systemd (which writes
// nothing): the conservative, externally-bounded contract is the default,
// and only an explicit order can extend it.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DrainKind is the retirement contract a drain falls under.
type DrainKind uint8

const (
	// DrainStop: the service is stopping (or the supervisor is about to
	// re-exec and is not serving meanwhile). Externally bounded.
	DrainStop DrainKind = iota
	// DrainSwap: a SIGHUP worker swap. The replacement worker is already
	// serving; this generation only has to finish its tails.
	DrainSwap
)

func (k DrainKind) String() string {
	if k == DrainSwap {
		return "swap"
	}
	return "stop"
}

// Flag names the operator knob that bounds this kind of drain, so a
// force-close WARN line points at the flag actually worth raising.
func (k DrainKind) Flag() string {
	if k == DrainSwap {
		return "-swap-drain-deadline"
	}
	return "-drain-deadline"
}

// DefaultDeadline is the built-in bound for this kind of drain.
func (k DrainKind) DefaultDeadline() time.Duration {
	if k == DrainSwap {
		return DefaultSwapDrainDeadline
	}
	return DefaultDrainDeadline
}

// DrainOrder is the one-shot instruction the supervisor writes to a
// worker's control pipe immediately before the retiring SIGTERM. It
// carries the deadline VALUE rather than a flag, so the supervisor stays
// the single source of truth and the wire is self-describing.
type DrainOrder struct {
	Kind     DrainKind
	Deadline time.Duration
}

// Normalize fills in the kind's default deadline when none was set, so
// the worker's drain bound and the supervisor's escalation bound are
// always derived from the same effective value.
func (o DrainOrder) Normalize() DrainOrder {
	if o.Deadline <= 0 {
		o.Deadline = o.Kind.DefaultDeadline()
	}
	return o
}

// RetireDeadline is how long the supervisor lets this retirement run
// before escalating to a process-group SIGKILL: the worker's own drain
// bound plus the headroom it needs to force-close and exit.
func (o DrainOrder) RetireDeadline() time.Duration {
	return o.Normalize().Deadline + RetireGrace
}

func (o DrainOrder) String() string {
	o = o.Normalize()
	return fmt.Sprintf("%s drain (deadline %s)", o.Kind, o.Deadline)
}

// drainOrderTag prefixes every order line so a stray byte on the pipe can
// never be mistaken for one.
const drainOrderTag = "drain"

// maxDrainOrder bounds the single read the worker performs. An order is
// one short line, well under PIPE_BUF, so the supervisor's single write
// is atomic and this single read returns it whole.
const maxDrainOrder = 128

// encode renders o as the one wire line the control pipe carries.
func (o DrainOrder) encode() []byte {
	o = o.Normalize()
	return fmt.Appendf(nil, "%s kind=%s deadline=%s\n", drainOrderTag, o.Kind, o.Deadline)
}

// parseDrainOrder decodes a wire line. Anything unrecognised is an error
// (the caller then falls back to stop semantics) rather than a guess.
func parseDrainOrder(b []byte) (DrainOrder, error) {
	f := strings.Fields(string(b))
	if len(f) != 3 || f[0] != drainOrderTag {
		return DrainOrder{}, fmt.Errorf("supervisor: malformed drain order %q", b)
	}
	var o DrainOrder
	switch f[1] {
	case "kind=" + DrainSwap.String():
		o.Kind = DrainSwap
	case "kind=" + DrainStop.String():
		o.Kind = DrainStop
	default:
		return DrainOrder{}, fmt.Errorf("supervisor: unknown drain order kind %q", f[1])
	}
	ds, ok := strings.CutPrefix(f[2], "deadline=")
	if !ok {
		return DrainOrder{}, fmt.Errorf("supervisor: drain order missing deadline: %q", f[2])
	}
	d, err := time.ParseDuration(ds)
	if err != nil {
		return DrainOrder{}, fmt.Errorf("supervisor: drain order deadline %q: %w", ds, err)
	}
	if d <= 0 {
		return DrainOrder{}, fmt.Errorf("supervisor: drain order deadline %s is not positive", d)
	}
	o.Deadline = d
	return o, nil
}

// ---------------------------------------------------------------------------
// Worker side
// ---------------------------------------------------------------------------

// Control is a worker's read end of its supervisor control pipe.
//
// The fd is kept RAW on purpose. Wrapping it in an *os.File would
// register it with the runtime netpoller, and a read on an empty pipe
// would then park the goroutine until it became readable — i.e. forever
// on a plain stop, where no order is ever written. That is exactly the
// path this must never block.
type Control struct {
	fd int // < 0 when this worker has no usable control pipe
}

// WorkerControl prepares the control pipe named by EnvControlFD. An unset
// or unusable fd yields a Control that always reports stop semantics,
// which is the safe default: a worker started outside a supervisor, or by
// a supervisor too old to speak drain orders, keeps the pre-existing
// behaviour.
func WorkerControl() *Control {
	fd, err := strconv.Atoi(os.Getenv(EnvControlFD))
	if err != nil {
		return &Control{fd: -1}
	}
	return newControl(fd)
}

// newControl takes ownership of fd for the process lifetime.
func newControl(fd int) *Control {
	if fd < 0 {
		return &Control{fd: -1}
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		return &Control{fd: -1}
	}
	// ExtraFiles hands the child its copies with O_CLOEXEC cleared;
	// re-arm it (SealInheritedFDs does this too, for the case where the
	// control pipe outlives an exec) so it never reaches an agent child.
	syscall.CloseOnExec(fd)
	return &Control{fd: fd}
}

// readFD is the raw-read seam (default syscall.Read). Tests stub it to
// drive the EINTR retry, which no test can provoke reliably otherwise.
var readFD = syscall.Read

// DrainOrder reports the retirement contract for the SIGTERM being
// handled right now. fallback is this worker's own stop bound
// (-drain-deadline), used whenever no order is pending — the plain-stop
// case, including a SIGTERM delivered straight to the cgroup by systemd.
//
// A non-nil error means the pipe or its payload was unusable; the
// returned order is still the safe stop default, so the caller can log
// and carry on. It never blocks: the supervisor's write completes before
// its kill(2), so a pending order is already buffered.
func (c *Control) DrainOrder(fallback time.Duration) (DrainOrder, error) {
	stop := DrainOrder{Kind: DrainStop, Deadline: fallback}.Normalize()
	if c.fd < 0 {
		return stop, nil
	}
	buf := make([]byte, maxDrainOrder)
	for {
		n, err := readFD(c.fd, buf)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue // interrupted before any byte moved; retry
		case errors.Is(err, syscall.EAGAIN):
			return stop, nil // nothing pending => a plain stop
		case err != nil:
			return stop, fmt.Errorf("supervisor: read drain order: %w", err)
		case n == 0:
			return stop, nil // supervisor closed the pipe without an order
		}
		o, perr := parseDrainOrder(buf[:n])
		if perr != nil {
			return stop, perr
		}
		return o.Normalize(), nil
	}
}
