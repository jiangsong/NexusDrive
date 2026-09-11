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

// Reserve registers a call for every key not already in flight, without
// running anything itself: the caller fetches the owned keys however it
// likes and hands the results to the returned resolve function. A key
// already in flight (via Do or another Reserve) is left alone and excluded
// from owned — whoever owns it will deliver its result the usual way, and a
// Do or Reserve for it here would just join as a waiter.
//
// This lets a caller batch several keys into one underlying fetch (e.g. one
// range request covering several cache blocks) while still sharing that
// fetch with any single-key Do that lands on one of the same keys.
func (f *flight[K, V]) Reserve(keys []K) (owned []K, resolve func(vals map[K]V, err error)) {
	f.mu.Lock()
	if f.m == nil {
		f.m = map[K]*flightCall[V]{}
	}
	owned = make([]K, 0, len(keys))
	calls := make([]*flightCall[V], 0, len(keys))
	for _, k := range keys {
		if _, ok := f.m[k]; ok {
			continue
		}
		c := &flightCall[V]{}
		c.wg.Add(1)
		f.m[k] = c
		owned = append(owned, k)
		calls = append(calls, c)
	}
	f.mu.Unlock()

	var once sync.Once
	resolve = func(vals map[K]V, err error) {
		once.Do(func() {
			for i, k := range owned {
				c := calls[i]
				c.val, c.err = vals[k], err
				c.wg.Done()
			}
			f.mu.Lock()
			for _, k := range owned {
				delete(f.m, k)
			}
			f.mu.Unlock()
		})
	}
	return owned, resolve
}
