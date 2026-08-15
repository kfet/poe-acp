//go:build unix

package supervisor

// One-deep binary swap with automatic rollback.
//
// Update = stage the new bytes as "<bin>.new" (same directory, so the
// final rename is atomic), rename the live file to "<bin>.prev", then
// rename "<bin>.new" over "<bin>". Renaming a running executable is
// safe — the running process keeps its open inode — and it sidesteps
// ETXTBSY, which writing in place would hit.
//
// The safety property is NOT the file dance: it is the existing
// "fork a worker, wait for its ready handshake, only then drain the
// old one" gate. SwapAndVerify wires the two together — if the new
// worker never comes ready, ".prev" goes back over the binary and a
// worker is forked from it again. The old worker was never drained, so
// service never breaks.
//
// Both sidecars exist ONLY inside the swap window. The moment the swap
// settles — new worker ready, or reverted because it was not — Cleanup
// unlinks whatever is left, so the directory holds exactly one file
// again: the binary. No state file, no version directory, no crash
// counter, no permanent ".prev". The one case this does not cover — a
// worker that comes ready and crash-loops an hour later — is recovered
// with the command that already exists:
//
//	poe-acp update --version <old-version>

import (
	"fmt"
	"os"
)

// StagedSuffix / PrevSuffix name the two sidecar files of a swap.
const (
	StagedSuffix = ".new"
	PrevSuffix   = ".prev"
)

// Swapper performs the rename dance for one binary path.
type Swapper struct {
	// Path is the live binary (the one the supervisor forks workers from).
	Path string
	// Rename is the rename seam (default os.Rename), overridden in tests.
	Rename func(oldpath, newpath string) error
}

// Staged is the path new bytes must be written to before Install.
func (s *Swapper) Staged() string { return s.Path + StagedSuffix }

// Prev is where the outgoing binary lives BETWEEN Install and Cleanup —
// that window only, never as a permanent artefact.
func (s *Swapper) Prev() string { return s.Path + PrevSuffix }

// Cleanup unlinks both sidecars. It is unconditional and idempotent: a
// missing file is not an error, so it is safe on every path — success,
// revert, a half-done Install, or a panic unwinding through a defer.
func (s *Swapper) Cleanup() {
	os.Remove(s.Staged())
	os.Remove(s.Prev())
}

func (s *Swapper) rename(oldpath, newpath string) error {
	if s.Rename != nil {
		return s.Rename(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

// Install moves the staged file into place, keeping the outgoing binary
// as Prev(). If the second rename fails the first is undone, so a failed
// Install always leaves the live binary intact.
func (s *Swapper) Install() error {
	if err := s.rename(s.Path, s.Prev()); err != nil {
		return fmt.Errorf("supervisor: keep previous binary: %w", err)
	}
	if err := s.rename(s.Staged(), s.Path); err != nil {
		// Put the old binary back so we never leave a hole where the
		// executable used to be.
		if rerr := s.rename(s.Prev(), s.Path); rerr != nil {
			return fmt.Errorf("supervisor: install staged binary: %w (restore failed: %v)", err, rerr)
		}
		return fmt.Errorf("supervisor: install staged binary: %w", err)
	}
	return nil
}

// Revert renames Prev() back over the live binary — the one-deep
// rollback. It is only meaningful straight after an Install.
func (s *Swapper) Revert() error {
	if err := s.rename(s.Prev(), s.Path); err != nil {
		return fmt.Errorf("supervisor: revert to %s: %w", s.Prev(), err)
	}
	return nil
}

// SwapAndVerify installs the staged binary and then asks bring to put a
// worker from it into service (fork + ready handshake — the caller's
// existing spawnReady). If bring fails, the previous binary is renamed
// back and bring is called once more, so the host ends up serving from
// whichever binary can actually come ready. reverted reports that the
// swap was undone; a non-nil error means the service needs attention.
// logf gets one greppable sentence per action.
//
// However it ends — including a panic unwinding through it — the swap
// leaves no sidecar behind: the deferred Cleanup is what guarantees the
// directory is back to a single binary.
func SwapAndVerify(s *Swapper, bring func() error, logf func(format string, args ...any)) (reverted bool, err error) {
	defer s.Cleanup()
	if err := s.Install(); err != nil {
		return false, err
	}
	if err := bring(); err == nil {
		return false, nil
	}
	if err := s.Revert(); err != nil {
		return true, err
	}
	if err := bring(); err != nil {
		return true, fmt.Errorf("supervisor: reverted to the previous binary but the worker is still not ready: %w", err)
	}
	logf("self-heal: REVERTED to the previous binary: worker never came ready — pin it with `poe-acp update --version <old>` if it recurs")
	return true, nil
}
