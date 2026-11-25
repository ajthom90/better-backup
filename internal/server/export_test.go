package server

// ExpireDeleted makes the delay of every deleted snapshot run out now.
func (s *Server) ExpireDeleted() {
	s.db.Exec(`UPDATE trash SET purge = 0`)
	s.kickBackground()
}

// SetBurstBytes changes how much new data counts as a burst, and how
// much it takes to judge compression, and returns a function that puts
// the old values back.
func SetBurstBytes(burst, dense int64) func() {
	b, d := detectBurstBytes, detectDenseBytes
	detectBurstBytes, detectDenseBytes = burst, dense
	return func() { detectBurstBytes, detectDenseBytes = b, d }
}
