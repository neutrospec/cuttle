package stats

import (
	"sync"
	"testing"
)

func TestLiveCounters(t *testing.T) {
	l := NewLive()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				l.Add(1, BytesTotal, "action", "direct", "direction", "up")
			}
		}()
	}
	wg.Wait()
	if got := l.Get(BytesTotal, "action", "direct", "direction", "up"); got != 8000 {
		t.Fatalf("got %d", got)
	}
	l.Add(5, BytesTotal, "action", "direct", "direction", "down")
	if l.Sum(BytesTotal) != 8005 {
		t.Errorf("sum %d", l.Sum(BytesTotal))
	}
	if k := Key(BytesTotal, "action", "direct", "direction", "up"); k != `bytes_total{action="direct",direction="up"}` {
		t.Errorf("key %s", k)
	}
	if l.Get(TunnelsActive) != 0 || len(l.Keys()) != 2 {
		t.Errorf("keys %v", l.Keys())
	}
}
