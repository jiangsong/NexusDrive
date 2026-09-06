package pool

import "context"

// candidates orders the members for placing a replica of pth: the members
// that already hold it come first, so an overwrite lands where the file
// lives; the rest follow in declaration order. Health, free space, weight
// and naming rules refine this order in later phases.
func (p *Pool) candidates(ctx context.Context, pth string) []*member {
	holding := map[string]bool{}
	rows, err := p.db.QueryContext(ctx, `SELECT member FROM replicas WHERE path = ?`, pth)
	if err == nil {
		for rows.Next() {
			var m string
			if rows.Scan(&m) == nil {
				holding[m] = true
			}
		}
		rows.Close()
	}
	out := make([]*member, 0, len(p.members))
	for _, m := range p.members {
		if holding[m.name] {
			out = append(out, m)
		}
	}
	for _, m := range p.members {
		if !holding[m.name] {
			out = append(out, m)
		}
	}
	return out
}
