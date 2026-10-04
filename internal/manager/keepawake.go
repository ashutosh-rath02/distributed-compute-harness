package manager

import (
	"log"
	"time"
)

// keepAwakeGrace keeps the manager's PC awake a little past the last
// running workload, so a job's next wave or a chat's follow-up question
// doesn't find it asleep.
const keepAwakeGrace = 2 * time.Minute

// keepAwakeTick holds the keep-awake request while any workload is being
// handed out or running, plus keepAwakeGrace. Queued work alone doesn't
// hold it: it may wait for hours on an unplugged phone, and a laptop
// manager on battery shouldn't stay up for that.
func (s *Server) keepAwakeTick(now time.Time) {
	if s.awake == nil {
		return
	}
	if s.Workloads.inFlight() {
		s.awakeUntil = now.Add(keepAwakeGrace)
	}
	if err := s.awake.Hold(now.Before(s.awakeUntil)); err != nil {
		log.Printf("manager: keep-awake: %v", err)
		s.awake = nil
	}
}

// KeepingAwake reports whether the manager is holding its PC awake.
func (s *Server) KeepingAwake() bool { return s.awake != nil && s.awake.Held() }
