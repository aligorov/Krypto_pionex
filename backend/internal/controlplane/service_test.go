package controlplane

import (
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/auth"
)

func TestDangerousCommands(t *testing.T) {
	t.Parallel()
	if dangerousCommand("scanner.run", nil) {
		t.Fatal("scanner run should not require confirmation")
	}
	if dangerousCommand("kill_switch.set", map[string]any{"enabled": true}) {
		t.Fatal("enabling kill switch should be immediate")
	}
	if !dangerousCommand("kill_switch.set", map[string]any{"enabled": false}) {
		t.Fatal("disabling kill switch must require confirmation")
	}
	if !dangerousCommand("grid.create", nil) {
		t.Fatal("grid creation must require confirmation")
	}
}

func TestRequiredRole(t *testing.T) {
	t.Parallel()
	if role := requiredRole("kill_switch.set", map[string]any{"enabled": false}); role != auth.RoleAdmin {
		t.Fatalf("expected admin, got %s", role)
	}
	if role := requiredRole("kill_switch.set", map[string]any{"enabled": true}); role != auth.RoleOperator {
		t.Fatalf("expected operator, got %s", role)
	}
}
