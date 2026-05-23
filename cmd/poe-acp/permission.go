package main

import (
	"fmt"
	"strings"

	"github.com/kfet/acp-kit/client"
)

// parsePermission resolves a --permission flag value to a
// client.PermissionPolicy backed by acp-kit's built-in policies.
// Recognised names: "" / "allow-all" / "allow", "read-only" / "readonly",
// "deny-all" / "deny". Anything else is an error.
func parsePermission(name string) (client.PermissionPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "allow-all", "allow":
		return client.PermissionFunc(client.AllowAllPermissions), nil
	case "read-only", "readonly":
		return client.PermissionFunc(client.ReadOnlyPermissions), nil
	case "deny-all", "deny":
		return client.PermissionFunc(client.DenyAllPermissions), nil
	default:
		return nil, fmt.Errorf("unknown policy %q (want allow-all|read-only|deny-all)", name)
	}
}
