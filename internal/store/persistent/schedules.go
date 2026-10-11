package persistent

import (
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"

	"home-harness/internal/domain"
)

// Schedules (manager/schedules.go). The bucket is created on first write,
// so a database from before schedules opens unchanged and reads as empty.
var schedulesBucket = []byte("schedules")

// UpsertSchedule stores one schedule: its definition and everything the
// scheduler needs to carry on after a restart (cursor, run count, queued
// slot, history) in a single write.
func (s *Store) UpsertSchedule(sc domain.Schedule) error {
	data, err := json.Marshal(sc)
	if err != nil {
		return fmt.Errorf("persistent: marshal schedule: %w", err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(schedulesBucket)
		if err != nil {
			return err
		}
		return b.Put([]byte(sc.ID), data)
	})
}

// DeleteSchedule removes a schedule.
func (s *Store) DeleteSchedule(id domain.ScheduleID) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(schedulesBucket)
		if b == nil {
			return nil
		}
		return b.Delete([]byte(id))
	})
}

// ListSchedules returns every stored schedule.
func (s *Store) ListSchedules() ([]domain.Schedule, error) {
	var out []domain.Schedule
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(schedulesBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, data []byte) error {
			var sc domain.Schedule
			if err := json.Unmarshal(data, &sc); err != nil {
				return fmt.Errorf("persistent: decode schedule: %w", err)
			}
			out = append(out, sc)
			return nil
		})
	})
	return out, err
}
