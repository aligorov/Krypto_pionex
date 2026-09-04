package autogrid

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

// TestIsSymbolOperationForbiddenError pins the v2.0.74 classifier on the raw
// 403 bodies seen in production: every body carrying a forbidden fragment
// (code, message or unparseable-envelope snippet) is a symbol state; every
// credential-shaped 403 and every non-403 error is not.
func TestIsSymbolOperationForbiddenError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "maintenance code in envelope",
			err:  &pionex.APIError{StatusCode: 403, Code: pionexSymbolMaintenanceReason, Message: "symbol maintenance"},
			want: true,
		},
		{
			name: "maintenance code inside invalid-json snippet",
			err:  &pionex.APIError{StatusCode: 403, Message: `invalid JSON response: {"reason":"` + pionexSymbolMaintenanceReason + `","result":false`},
			want: true,
		},
		{
			name: "plain Operation is forbidden message",
			err:  &pionex.APIError{StatusCode: 403, Message: "Operation is forbidden"},
			want: true,
		},
		{
			name: "truncated operation message",
			err:  &pionex.APIError{StatusCode: 403, Message: "Operation ..."},
			want: false,
		},
		{
			name: "forbidden fragment inside unparseable snippet",
			err:  &pionex.APIError{StatusCode: 403, Message: `invalid JSON response: {"data":null,"code":403,"reason":"P_TRADING_BOT_OPERATION_IS_FORBIDDEN"}`},
			want: true,
		},
		{
			name: "forbidden in lowercase message",
			err:  &pionex.APIError{StatusCode: 403, Message: "this operation is Forbidden for the symbol"},
			want: true,
		},
		{
			name: "api key invalid is credential not symbol",
			err:  &pionex.APIError{StatusCode: 403, Code: "P_API_KEY_INVALID", Message: "api key is invalid"},
			want: false,
		},
		{
			name: "signature refusal is credential not symbol",
			err:  &pionex.APIError{StatusCode: 403, Message: "signature verification failed"},
			want: false,
		},
		{
			name: "forbidden text with api key marker stays credential",
			err:  &pionex.APIError{StatusCode: 403, Message: "operation is forbidden: api key has no bot permission"},
			want: false,
		},
		{
			name: "non-403 with forbidden text is not a symbol refusal",
			err:  &pionex.APIError{StatusCode: 500, Message: "operation is forbidden"},
			want: false,
		},
		{
			name: "wrapped error still classified",
			err:  fmt.Errorf("create remote Pionex futures grid: %w", &pionex.APIError{StatusCode: 403, Message: "Operation is forbidden"}),
			want: true,
		},
		{
			name: "plain error never classified",
			err:  errors.New("connection reset"),
			want: false,
		},
	}
	for _, tc := range cases {
		if got := isSymbolOperationForbiddenError(tc.err); got != tc.want {
			t.Fatalf("%s: isSymbolOperationForbiddenError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestForbiddenClassifierThroughClient feeds the raw HTTP bodies through the
// real client so the classification covers the exact wire shapes: truncated
// JSON, numeric-code envelopes and plain-text 403s all surface their reason
// inside APIError.Message or Code.
func TestForbiddenClassifierThroughClient(t *testing.T) {
	bodies := map[string]string{
		"truncated maintenance object": `{"reason":"` + pionexSymbolMaintenanceReason + `","result":false`,
		"operation envelope":           `{"result":false,"message":"Operation is forbidden"}`,
		"numeric code body":            `{"data":null,"code":403,"reason":"P_TRADING_BOT_OPERATION_IS_FORBIDDEN"}`,
	}
	for name, body := range bodies {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(body))
		}))
		client := pionex.NewClient(server.URL, "key", "secret")
		_, err := client.CheckFuturesGridParams(context.Background(), pionex.NativeFuturesGridCreateParams{})
		server.Close()
		if err == nil {
			t.Fatalf("%s: expected a 403 error", name)
		}
		if !isSymbolOperationForbiddenError(err) {
			t.Fatalf("%s: body %q must classify as symbol-forbidden, got %v", name, body, err)
		}
	}

	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"result":false,"code":"P_API_KEY_INVALID","message":"api key is invalid"}`))
	}))
	authClient := pionex.NewClient(authServer.URL, "key", "secret")
	_, err := authClient.CheckFuturesGridParams(context.Background(), pionex.NativeFuturesGridCreateParams{})
	authServer.Close()
	if err == nil {
		t.Fatalf("expected a 403 auth error")
	}
	if isSymbolOperationForbiddenError(err) {
		t.Fatalf("credential 403 must not classify as symbol-forbidden, got %v", err)
	}
	if !strings.Contains(err.Error(), "P_API_KEY_INVALID") {
		t.Fatalf("auth refusal must keep its exchange code for operators, got %v", err)
	}
}
