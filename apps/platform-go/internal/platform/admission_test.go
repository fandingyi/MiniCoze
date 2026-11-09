package platform

import "testing"

func TestChannelAdmissionReleaseAndIsolation(t *testing.T) {
	a := channelAdmission{active: map[string]int{}, limit: 1}
	release, ok := a.acquire("a")
	if !ok {
		t.Fatal("first admission")
	}
	if _, ok := a.acquire("a"); ok {
		t.Fatal("limit ignored")
	}
	releaseB, ok := a.acquire("b")
	if !ok {
		t.Fatal("other channel blocked")
	}
	release()
	release()
	releaseB()
	if len(a.active) != 0 {
		t.Fatal("admission entries leaked")
	}
	release, ok = a.acquire("a")
	if !ok {
		t.Fatal("slot not reusable")
	}
	release()
}
