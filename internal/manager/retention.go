package manager

import (
	"log"
	"sort"
	"time"

	"home-harness/internal/domain"
)

// Finished workloads are not kept forever: every task, and every chat
// through the OpenAI-compatible API, leaves a record (request, output,
// up to 64 KiB each) in memory and in the store. A finished workload is
// forgotten once it is older than finishedRetention or beyond the newest
// finishedKeep; chats keep only the newest chatKeep, since their records
// hold whole conversations. Kept regardless: anything not finished, a
// workload its restart policy will run again, and batch-job attempts
// (a job's state is derived from them).
const (
	finishedRetention = 7 * 24 * time.Hour
	finishedKeep      = 1000
	chatKeep          = 100
	expireEvery       = time.Minute
)

// expireFinished removes the finished workloads the rules above let go,
// returning their IDs.
func (wr *WorkloadRegistry) expireFinished(now time.Time, retention time.Duration, keep, keepChats int) []domain.WorkloadID {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	type finished struct {
		id   domain.WorkloadID
		at   time.Time
		chat bool
	}
	var done []finished
	for id, rec := range wr.workloads {
		state := rec.Status.State
		switch state {
		case domain.WorkloadCompleted, domain.WorkloadFailed, domain.WorkloadCanceled, domain.WorkloadUnknown:
		default:
			continue
		}
		if rec.Workload.Job != "" || rec.Workload.RestartPolicy.WantsRestartAfter(state) {
			continue
		}
		at := rec.Status.FinishedAt
		if at.IsZero() {
			// Ended without a finish time (canceled while queued, or lost in
			// a manager restart): its age counts from when this first saw it.
			if rec.finishedSeen.IsZero() {
				rec.finishedSeen = now
			}
			at = rec.finishedSeen
		}
		done = append(done, finished{id, at, rec.Workload.EffectiveCapability() == capLLMChat})
	}
	sort.Slice(done, func(i, j int) bool { return done[i].at.After(done[j].at) }) // newest first
	var expired []domain.WorkloadID
	kept, chats := 0, 0
	for _, f := range done {
		drop := now.Sub(f.at) > retention || kept >= keep
		if f.chat {
			drop = drop || chats >= keepChats
		}
		if drop {
			delete(wr.workloads, f.id)
			expired = append(expired, f.id)
			continue
		}
		kept++
		if f.chat {
			chats++
		}
	}
	return expired
}

// expireWorkloads forgets the finished workloads past retention, in
// memory and in the store.
func (s *Server) expireWorkloads() {
	ids := s.Workloads.expireFinished(time.Now(), finishedRetention, finishedKeep, chatKeep)
	if len(ids) == 0 {
		return
	}
	if s.store != nil {
		if err := s.store.DeleteWorkloads(ids); err != nil {
			log.Printf("manager: forget %d finished workloads: %v", len(ids), err)
			return
		}
	}
	log.Printf("manager: forgot %d finished workloads past retention", len(ids))
}
