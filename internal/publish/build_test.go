package publish

import "testing"

func TestBuildIDIsStable(t *testing.T) {
	a, err := BuildID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := BuildID()
	if len(a) != 64 || a != b {
		t.Errorf("BuildID: %q, then %q", a, b)
	}
}
