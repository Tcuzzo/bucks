package tui

import (
	"strings"
	"testing"
)

// TestDashboardShowsBuckWordmarkUpTop locks the operator's requirement: the BUCKS block
// wordmark renders UP TOP on the dashboard — above the locator and all content — on both the
// waiting and the populated frames.
func TestDashboardShowsBuckWordmarkUpTop(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    DashboardModel
	}{
		{"read-only", NewDashboard()},
		{"chat", NewDashboardWithChat(nil)},
	} {
		v := tc.m.View()
		banner := strings.Index(v, "██████")
		if banner < 0 {
			t.Fatalf("%s: dashboard must render the BUCKS block wordmark", tc.name)
		}
		locator := strings.Index(v, "live dashboard")
		if locator < 0 || banner > locator {
			t.Errorf("%s: BUCKS wordmark must be UP TOP, above the locator (banner@%d, locator@%d)",
				tc.name, banner, locator)
		}
		// The literal brand name is still present (under the art) for plain-text surfaces.
		if !strings.Contains(v, "BUCKS") {
			t.Errorf("%s: dashboard must still carry the literal BUCKS name", tc.name)
		}
	}
}
