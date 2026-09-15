package trigger

// changesSeen reports how many changes the matcher has consumed, so a test
// can wait for the table to reflect what it fed in.
func (e *Engine) changesSeen() int64 { return e.seen.Load() }
