package auth

import "testing"

func TestAllowsConfiguredUser(t *testing.T) {
	a := New(12345)
	if !a.Allows(12345) {
		t.Error("expected Allows(12345) = true")
	}
}

func TestRejectsOthers(t *testing.T) {
	a := New(12345)
	for _, id := range []int64{0, 1, 12344, 12346, -1} {
		if a.Allows(id) {
			t.Errorf("Allows(%d) = true, want false", id)
		}
	}
}
