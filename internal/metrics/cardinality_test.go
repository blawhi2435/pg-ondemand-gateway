package metrics

import "testing"

// forbiddenLabels are unbounded-cardinality dimensions design §9.5
// explicitly forbids on any metric.
var forbiddenLabels = []string{"user", "database", "client_ip", "sni", "hostname"}

func TestMetrics_NoUnboundedCardinalityLabels(t *testing.T) {
	m := New()

	// Exercise every metric so its label set actually appears in Gather.
	m.IncActive("tenant1")
	m.IncConnectionsTotal("tenant1", "ok")
	m.IncHandshakeError("unknown_sni")
	m.ObserveBackendDialSeconds("tenant1", 0.01)
	m.AddBytes("tenant1", "in", 100)
	m.SetCertExpirySeconds(1)
	m.SetDraining(false)
	m.SetNofileLimit(1)
	m.SetRouteTableEntries(1)
	m.SetRouteTableLastSyncSeconds(1)

	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, fam := range families {
		for _, metric := range fam.GetMetric() {
			for _, lp := range metric.GetLabel() {
				for _, forbidden := range forbiddenLabels {
					if lp.GetName() == forbidden {
						t.Errorf("metric %s carries forbidden label %q (unbounded cardinality)", fam.GetName(), forbidden)
					}
				}
			}
		}
	}
}
