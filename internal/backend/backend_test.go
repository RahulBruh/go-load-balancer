package backend

import (
	"net/url"
	"sync"
	"testing"
)

func newTest() *Backend {
	u, _ := url.Parse("http://localhost:1")
	return New(u, 0)
}

func TestNewDefaults(t *testing.T) {
	b := newTest()
	if !b.Alive() || b.Weight != 1 || b.Name() != "localhost:1" {
		t.Errorf("got alive=%v weight=%d name=%q", b.Alive(), b.Weight, b.Name())
	}
}

func TestThresholds(t *testing.T) {
	b := newTest()
	if b.RecordFailure(2) || !b.Alive() {
		t.Fatal("one failure took the backend down with fall=2")
	}
	if !b.RecordFailure(2) || b.Alive() {
		t.Fatal("second failure should take the backend down and report a change")
	}
	if b.RecordFailure(2) {
		t.Fatal("already-down backend reported another change")
	}
	if b.RecordSuccess(2) || b.Alive() {
		t.Fatal("one success brought the backend back with rise=2")
	}
	if !b.RecordSuccess(2) || !b.Alive() {
		t.Fatal("second success should bring the backend back")
	}
}

func TestSuccessResetsFailureStreak(t *testing.T) {
	b := newTest()
	b.RecordFailure(2)
	b.RecordSuccess(1)
	b.RecordFailure(2)
	if !b.Alive() {
		t.Fatal("non-consecutive failures took the backend down")
	}
}

func TestAcquireReleaseIsIdempotent(t *testing.T) {
	b := newTest()
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := b.Acquire()
			release()
			release()
		}()
	}
	wg.Wait()
	if b.Active() != 0 {
		t.Errorf("active = %d, want 0", b.Active())
	}
}
