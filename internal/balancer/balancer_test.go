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
	for _, name := range []string{"round_robin"} {
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
