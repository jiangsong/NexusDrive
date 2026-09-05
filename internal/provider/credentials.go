package provider

import "sync"

// TokenPersister saves rotated credentials before another API request uses
// them. Implementations must not retain or mutate the supplied map.
type TokenPersister func(map[string]string) error

type TokenPersistenceSetter interface{ SetTokenPersister(TokenPersister) }

// TokenPersistence retains a failed save in memory and retries it before the
// next authenticated request, rather than refreshing an already rotated token.
type TokenPersistence struct {
	mu      sync.Mutex
	save    TokenPersister
	pending map[string]string
}

func (p *TokenPersistence) SetTokenPersister(save TokenPersister) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.save = save
}

func (p *TokenPersistence) SaveTokens(fields map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = map[string]string{}
	}
	for k, v := range fields {
		if v != "" {
			p.pending[k] = v
		}
	}
	return p.flush()
}

func (p *TokenPersistence) FlushTokens() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.flush()
}

func (p *TokenPersistence) flush() error {
	if len(p.pending) == 0 || p.save == nil {
		return nil
	}
	copy := make(map[string]string, len(p.pending))
	for k, v := range p.pending {
		copy[k] = v
	}
	if err := p.save(copy); err != nil {
		return err
	}
	p.pending = nil
	return nil
}
