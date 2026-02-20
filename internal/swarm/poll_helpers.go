package swarm

import "time"

func (s *IssueService) deadline(timeoutSec int) time.Time {
	sec := timeoutSec
	if sec <= 0 {
		sec = s.defaultTimeoutSec
	}
	if sec <= 0 {
		sec = 3600
	}
	return time.Now().Add(time.Duration(sec) * time.Second)
}

func timeExpired(dl time.Time) bool {
	return time.Now().After(dl)
}

func sleepPoll() {
	time.Sleep(200 * time.Millisecond)
}
