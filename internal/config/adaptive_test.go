package config

import "testing"

func TestAdaptiveTargetsDefaultAndBoundTheirAdmission(t *testing.T) {
	for _, mode := range []string{"batch", "speculate"} {
		cfg, err := loadDaemonString(t, profileTestBase+`target "adaptive" branch="next" { mode "`+mode+`"; adaptive {}; }`)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, target := range cfg.Targets {
			if target.Name == "adaptive" {
				found = true
				if target.Adaptive.Start != 1 || target.Adaptive.Successes != 4 {
					t.Fatalf("adaptive defaults: %+v", target.Adaptive)
				}
			}
		}
		if !found {
			t.Fatal("adaptive target missing")
		}
	}
	if _, err := loadDaemonString(t, profileTestBase+`target "adaptive" branch="next" {adaptive {};}`); err == nil {
		t.Fatal("serial accepted adaptive")
	}
}
