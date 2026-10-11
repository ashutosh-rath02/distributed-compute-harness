package persistent

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"home-harness/internal/domain"
)

// BenchmarkUpsertWorkload shows what one durable write costs, alone and
// with 16 writers arriving together (group commit).
func BenchmarkUpsertWorkload(b *testing.B) {
	for _, writers := range []int{1, 16} {
		b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "m.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			b.ResetTimer()
			var wg sync.WaitGroup
			per := b.N/writers + 1
			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < per; i++ {
						id := domain.WorkloadID(fmt.Sprintf("w-%d-%d", w, i))
						pw := domain.PersistedWorkload{}
						pw.Workload.ID = id
						if err := s.UpsertWorkload(pw); err != nil {
							b.Error(err)
							return
						}
					}
				}()
			}
			wg.Wait()
		})
	}
}
