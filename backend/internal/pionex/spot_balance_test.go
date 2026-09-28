package pionex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBotFundingSpotContract(t *testing.T) {
	for _, tc := range []struct {
		name, data, want string
		bad              bool
	}{
		{"free excludes frozen", `{"balances":[{"coin":"USDT","free":"253.12","frozen":"900"}]}`, "253.12", false},
		{"explicit zero", `{"balances":[{"coin":"USDT","free":"0"}]}`, "0", false},
		{"empty", `{"balances":[]}`, "0", false},
		{"other coin", `{"balances":[{"coin":"BTC","free":"5"}]}`, "0", false},
		{"missing list", `{}`, "", true},
		{"null list", `{"balances":null}`, "", true},
		{"missing free", `{"balances":[{"coin":"USDT"}]}`, "", true},
		{"negative", `{"balances":[{"coin":"USDT","free":"-1"}]}`, "", true},
		{"malformed", `{"balances":[{"coin":"USDT","free":"oops"}]}`, "", true},
		{"duplicate", `{"balances":[{"coin":"USDT","free":"1"},{"coin":"USDT","free":"2"}]}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/account/balances" {
					t.Errorf("wrong funding endpoint: %s", r.URL)
				}
				if r.Header.Get("PIONEX-KEY") == "" || r.Header.Get("PIONEX-SIGNATURE") == "" || r.URL.Query().Get("timestamp") == "" {
					t.Error("missing signed request credentials")
				}
				fmt.Fprintf(w, `{"result":true,"data":%s}`, tc.data)
			}))
			defer s.Close()
			v, err := NewClient(s.URL, "test-key", "test-secret").GetBotFundingUSDT(context.Background())
			if (err != nil) != tc.bad {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.bad && v.String() != tc.want {
				t.Fatalf("got %s, want %s", v, tc.want)
			}
		})
	}
}
