package dist

import (
	"os"
	"path/filepath"
	"time"
)

// Status is the snapshot a host leaves next to its config after every
// reconcile. It is a plain file: no endpoint, no push, no server.
// `poe-acp fleet` ssh-cats it; a sleeping node shows as unreachable,
// which is the truth.
type Status struct {
	Time     string   `json:"time"`
	Host     string   `json:"host"`
	PoeACP   string   `json:"poe_acp"`
	Fir      string   `json:"fir,omitempty"`
	Lock     Lock     `json:"lock"`
	LockETag string   `json:"lock_etag,omitempty"`
	Applied  bool     `json:"applied"`
	Actions  []string `json:"actions,omitempty"`
	Drift    []string `json:"drift,omitempty"`
}

// StatusPath is where a bot's status snapshot lives, given its config
// directory.
func StatusPath(dir string) string { return filepath.Join(dir, "status.json") }

func writeStatus(opts *Options, res *Result) error {
	host, _ := os.Hostname()
	return writeJSON(StatusPath(opts.Dir), Status{
		Time:     time.Now().UTC().Format(time.RFC3339),
		Host:     host,
		PoeACP:   opts.Version,
		Fir:      res.FirVer,
		Lock:     res.Lock,
		LockETag: res.ETag,
		Applied:  opts.Apply,
		Actions:  res.Actions,
		Drift:    res.Drift,
	})
}
