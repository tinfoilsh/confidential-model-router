package billing

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

const (
	testUsageContextSecret = "router-test-usage-secret"
	testAPIKey             = "tk_customer"
)

func TestCustomerRequestCount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		secret  string
		change  func(*usagereporting.Context)
		want    int64
		wantErr string
	}{
		{name: "harness already billed", secret: testUsageContextSecret, want: 0},
		{name: "harness asks to bill", secret: testUsageContextSecret, change: func(c *usagereporting.Context) { c.BillCustomerRequest = true }, want: 1},
		{name: "forged signature", secret: "another-secret", wantErr: "invalid usage context signature"},
		{name: "wrong service", secret: testUsageContextSecret, change: func(c *usagereporting.Context) { c.ParentService = usagereporting.ServiceWebsearch }, wantErr: "may not suppress"},
		{name: "nested parent", secret: testUsageContextSecret, change: func(c *usagereporting.Context) { c.Depth = 2 }, wantErr: "not a direct call"},
		{name: "missing key binding", secret: testUsageContextSecret, change: func(c *usagereporting.Context) { c.APIKeyHash = "" }, wantErr: "names no API key"},
		{name: "wrong key binding", secret: testUsageContextSecret, change: func(c *usagereporting.Context) { c.APIKeyHash = usagereporting.HashAPIKey("tk_other") }, wantErr: "different API key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := usagereporting.Context{
				ContextID:     "run-1",
				RootRequestID: "run-1",
				ParentService: harnessService,
				Depth:         harnessCallDepth,
				APIKeyHash:    usagereporting.HashAPIKey(testAPIKey),
				IssuedAt:      time.Now(),
			}
			if tc.change != nil {
				tc.change(&usage)
			}
			h := http.Header{}
			if err := usagereporting.SetHeaders(h, usage, tc.secret); err != nil {
				t.Fatal(err)
			}
			got, err := CustomerRequestCount(h, testAPIKey, testUsageContextSecret)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("count = %d, %v, want %d", got, err, tc.want)
			}
		})
	}
}

func TestCustomerRequestCountWithoutUsageContext(t *testing.T) {
	got, err := CustomerRequestCount(http.Header{}, testAPIKey, testUsageContextSecret)
	if err != nil || got != 1 {
		t.Fatalf("count = %d, %v, want 1", got, err)
	}
	if got := CustomerRequests(context.Background()); got != 1 {
		t.Fatalf("count without a resolved context = %d, want 1", got)
	}
	if got := CustomerRequests(WithCustomerRequests(context.Background(), 0)); got != 0 {
		t.Fatalf("resolved count = %d, want 0", got)
	}
}
