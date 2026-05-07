package debuglog

import "os/exec"

func newCmd(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }
