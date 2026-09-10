package authz

import "testing"

func TestAllowAll_AlwaysAllows(t *testing.T) {
	a := NewAllowAll()

	cases := []struct{ cluster, user, database, clientIP string }{
		{"tenant1", "app_rw", "orders", "10.0.0.1"},
		{"tenant2", "analyst_ro", "reports", "10.0.0.2"},
		{"", "", "", ""},
	}
	for _, c := range cases {
		d := a.Check(c.cluster, c.user, c.database, c.clientIP)
		if !d.Allow {
			t.Errorf("Check(%+v) = %+v, want Allow=true", c, d)
		}
	}
}
