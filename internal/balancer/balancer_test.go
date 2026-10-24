package balancer

import (
	"fmt"
	"net/url"
	"testing"

	"github.com/RahulBruh/go-load-balancer/internal/backend"
)

func pool(weights ...int) []*backend.Backend {
	bs := make([]*backend.Backend, len(weights))
	for i, w := range weights {
		u, _ := url.Parse(fmt.Sprintf("http://b%d:80", i))
		bs[i] = backend.New(u, w)
	}
	return bs
}

func names(s Strategy, bs []*backend.Backend, n int) string {
	out := ""
	for range n {
		out += s.Next(bs).Name()[:2]
	}
	return out
}

func TestEmptyCandidatesReturnNil(t *testing.T) {
	for _, name := range []string{"round_robin", "weighted_round_robin", "least_connections"} {
		s, err := New(name)
		if err != nil {
			t.Fatal(err)
		}
		if s.Next(nil) != nil {
			t.Errorf("%s returned a backend from an empty pool", name)
		}
	}
}

func TestRoundRobinOrder(t *testing.T) {
	if got := names(&RoundRobin{}, pool(1, 1, 1), 6); got != "b0b1b2b0b1b2" {
		t.Errorf("order = %s", got)
	}
}

func TestUnknownStrategy(t *testing.T) {
	if _, err := New("random"); err == nil {
		t.Error("expected an error")
	}
}

func TestWeightedRoundRobinIsSmooth(t *testing.T) {
	// nginx's documented sequence for weights 5, 1, 1.
	if got := names(NewWeightedRoundRobin(), pool(5, 1, 1), 7); got != "b0b0b1b0b2b0b0" {
		t.Errorf("order = %s", got)
	}
}

func TestWeightedRoundRobinRatio(t *testing.T) {
	bs := pool(3, 2, 1)
	s := NewWeightedRoundRobin()
	counts := map[string]int{}
	for range 600 {
		counts[s.Next(bs).Name()]++
	}
	want := map[string]int{"b0:80": 300, "b1:80": 200, "b2:80": 100}
	for name, n := range want {
		if counts[name] != n {
			t.Errorf("%s served %d, want %d", name, counts[name], n)
		}
	}
}

func TestWeightedRoundRobinAdaptsToSmallerPool(t *testing.T) {
	bs := pool(5, 1, 1)
	s := NewWeightedRoundRobin()
	names(s, bs, 3)
	// b0 drops out: the rest still alternate.
	if got := names(s, bs[1:], 4); got != "b1b2b1b2" && got != "b2b1b2b1" {
		t.Errorf("order = %s", got)
	}
}

func TestLeastConnectionsPicksIdlest(t *testing.T) {
	bs := pool(1, 1, 1)
	r0 := bs[0].Acquire()
	r1 := bs[1].Acquire()
	bs[1].Acquire()
	s := &LeastConnections{}
	for range 5 {
		if got := s.Next(bs); got != bs[2] {
			t.Fatalf("picked %s, want the idle b2", got.Name())
		}
	}
	r0()
	r1()
	// Now b0 has 0, b1 has 1, b2 has 0: never b1.
	for range 10 {
		if s.Next(bs) == bs[1] {
			t.Fatal("picked the busiest backend")
		}
	}
}

func TestLeastConnectionsRespectsWeight(t *testing.T) {
	bs := pool(4, 1)
	for range 3 {
		bs[0].Acquire() // 3 in flight / weight 4
	}
	bs[1].Acquire() // 1 in flight / weight 1
	if got := (&LeastConnections{}).Next(bs); got != bs[0] {
		t.Errorf("picked %s, want b0 (0.75 per weight vs 1)", got.Name())
	}
}

func TestLeastConnectionsSpreadsTies(t *testing.T) {
	bs := pool(1, 1, 1)
	if got := names(&LeastConnections{}, bs, 6); got != "b0b1b2b0b1b2" {
		t.Errorf("order = %s", got)
	}
}
