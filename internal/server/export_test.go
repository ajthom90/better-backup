package server

import "time"

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

// SetBrowseWaitUnseen shortens the wait for an agent that never asks
// for questions, and returns how to put it back.
func SetBrowseWaitUnseen(d time.Duration) func() {
	old := browseWaitUnseen
	browseWaitUnseen = d
	return func() { browseWaitUnseen = old }
}

// SetInContainer says whether the server believes it runs in a
// container, and returns how to put the real answer back.
func SetInContainer(v bool) func() {
	old := inContainer
	inContainer = func() bool { return v }
	return func() { inContainer = old }
}
