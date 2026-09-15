package agent

// setAppendFault arms the append fault hook, so a test can make agent.db
// refuse audit rows without touching the file system.
func (s *Store) setAppendFault(f func() error) { s.appendFault = f }
