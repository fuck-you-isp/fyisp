package metrics

import (
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

// TestStorage: rounds by outcome, attempts by target name, bytes and the
// latest transfer speed.
func TestStorage(t *testing.T) {
	s := NewStorage()
	g := 8e7
	c := model.StorageCampaign{Targets: []model.StorageCampaignTarget{{ID: "t_1", Name: "Lab"}}}
	s.Observe(c, model.StorageRound{Outcome: model.RoundFailed, BytesDown: 1 << 20}, []model.StorageAttempt{
		{Target: "t_1", Phase: "cold", Class: "ok"}, {Target: "t_1", Phase: "cold", Class: "tls_timeout"},
		{Target: "t_1", Phase: "down", Class: "ok", GoodputBps: &g},
	})
	s.Observe(c, model.StorageRound{Outcome: model.RoundSkipped}, nil)
	reg := prometheus.NewRegistry()
	reg.MustRegister(s)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			k := mf.GetName()
			for _, l := range m.GetLabel() {
				k += "," + l.GetName() + "=" + l.GetValue()
			}
			if m.Counter != nil {
				got[k] = m.GetCounter().GetValue()
			} else {
				got[k] = m.GetGauge().GetValue()
			}
		}
	}
	for k, v := range map[string]float64{
		"fyisp_storage_rounds_total,outcome=failed":                            1,
		"fyisp_storage_rounds_total,outcome=skipped":                           1,
		"fyisp_storage_attempts_total,class=tls_timeout,phase=cold,target=Lab": 1,
		"fyisp_storage_attempts_total,class=ok,phase=cold,target=Lab":          1,
		"fyisp_storage_bytes_total,direction=down":                             1 << 20,
		"fyisp_storage_goodput_bits_per_second,direction=down,target=Lab":      g,
	} {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}
