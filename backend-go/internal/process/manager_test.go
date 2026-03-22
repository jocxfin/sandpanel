package process

import "testing"

func TestFinalRCONTravelTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		bootstrap string
		full      string
		want      string
	}{
		{
			name:      "mods only does not require extra travel",
			bootstrap: "Town?Scenario=Scenario_Town_Checkpoint_Security",
			full:      "Town?Scenario=Scenario_Town_Checkpoint_Security",
			want:      "",
		},
		{
			name:      "mutators require final travel",
			bootstrap: "Town?Scenario=Scenario_Town_Checkpoint_Security",
			full:      "Town?Scenario=Scenario_Town_Checkpoint_Security?Mutators=MyMut",
			want:      "Town?Scenario=Scenario_Town_Checkpoint_Security?Mutators=MyMut",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := finalRCONTravelTarget(tt.bootstrap, tt.full); got != tt.want {
				t.Fatalf("finalRCONTravelTarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRequiresReadinessTracking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		hasMods            bool
		pendingFinalTravel string
		want               bool
	}{
		{
			name:               "mods only still requires readiness tracking",
			hasMods:            true,
			pendingFinalTravel: "",
			want:               true,
		},
		{
			name:               "mutators without mods requires readiness tracking",
			hasMods:            false,
			pendingFinalTravel: "Town?Scenario=Scenario_Town_Checkpoint_Security?Mutators=MyMut",
			want:               true,
		},
		{
			name:               "no mods and no final travel does not require tracking",
			hasMods:            false,
			pendingFinalTravel: "",
			want:               false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := requiresReadinessTracking(tt.hasMods, tt.pendingFinalTravel); got != tt.want {
				t.Fatalf("requiresReadinessTracking() = %v, want %v", got, tt.want)
			}
		})
	}
}
