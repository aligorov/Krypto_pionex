package auth

import "testing"

func TestValidatePassword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		password string
		valid    bool
	}{
		{name: "strong", password: "Correct-Horse-2026!", valid: true},
		{name: "too short", password: "Short1!", valid: false},
		{name: "no symbol", password: "LongPassword2026", valid: false},
		{name: "no digit", password: "LongPassword!!", valid: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validatePassword(test.password)
			if test.valid && err != nil {
				t.Fatalf("expected valid password: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("expected password validation error")
			}
		})
	}
}

func TestPrincipalScopeIsStrictForMCP(t *testing.T) {
	t.Parallel()
	principal := Principal{Role: RoleAdmin, ActorType: "MCP", Scopes: []string{"mcp:read"}}
	if !principal.HasScope("mcp:read") {
		t.Fatal("expected read scope")
	}
	if principal.HasScope("mcp:write") {
		t.Fatal("admin role must not bypass an MCP token's scopes")
	}
}
