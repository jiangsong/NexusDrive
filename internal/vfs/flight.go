package vfs

import "sync"

// flight collapses concurrent identical requests into one call, so eight
// readers faulting the same block issue one provider request. It is a small
// generic singleflight; the standard one is not in the module graph.
type flight[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]*flightCall[V]
}

type flightCall[V any] struct {
	wg  sync.WaitGroup
	val V
	err error
}

// Do runs fn once per key at a time and shares the result with every waiter.
func (f *flight[K, V]) Do(key K, fn func() (V, error)) (V, error) {
	f.mu.Lock()
	if f.m == nil {
		f.m = map[K]*flightCall[V]{}
	}
	if c, ok := f.m[key]; ok {
		f.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &flightCall[V]{}
	c.wg.Add(1)
	f.m[key] = c
	f.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	f.mu.Lock()
	delete(f.m, key)
	f.mu.Unlock()
	return c.val, c.err
}
