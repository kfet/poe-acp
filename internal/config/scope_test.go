package config

import "testing"

func TestValidate_SessionScopeAndCwdMode(t *testing.T) {
	for _, c := range []Config{
		{}, {SessionScope: "thread"}, {SessionScope: "thread_user"},
		{CwdMode: "local"}, {CwdMode: "agent"},
	} {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	for _, c := range []Config{{SessionScope: "user"}, {CwdMode: "remote"}} {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v: want error", c)
		}
	}
}
