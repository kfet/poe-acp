package httpsrv

import (
	"fmt"
	"time"

	"github.com/kfet/acp-kit/client"
)

// A turn the RELAY cuts short must be able to say why. Both causes below
// wrap one of acp-kit's sentinels — the same ones slack-acp and zulip-acp
// classify on — so `errors.Is(context.Cause(ctx), client.ErrNoProgress)`
// works across all three relays, while `Error()` carries the sentence the
// user actually reads.
//
// The sentence lives HERE, not in the router, because this is the layer
// that owns the timeouts and therefore the only one that knows the
// numbers. The router renders whatever it is handed (see turnEndNote).

// noProgressCause is client.ErrNoProgress carrying the window that
// expired: the agent produced no output and no tool activity for that
// long, so the idle-write backstop cut the turn.
type noProgressCause struct{ window time.Duration }

func (c noProgressCause) Error() string {
	return fmt.Sprintf("no output or tool activity from the agent for %s — it looks wedged", c.window)
}

func (c noProgressCause) Unwrap() error { return client.ErrNoProgress }

// turnCeilingCause is client.ErrTurnCeiling carrying the operator's
// opt-in absolute cap. Unlike noProgressCause this fires on a turn that
// may have been working the whole time, so it names the knob rather than
// implying the agent misbehaved.
type turnCeilingCause struct{ ceiling time.Duration }

func (c turnCeilingCause) Error() string {
	return fmt.Sprintf("this turn hit the relay's %s ceiling (-turn-timeout)", c.ceiling)
}

func (c turnCeilingCause) Unwrap() error { return client.ErrTurnCeiling }
