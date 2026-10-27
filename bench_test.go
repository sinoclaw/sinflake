package sinflake

import (
	"context"
	"testing"
	"time"
)

func BenchmarkSinflakeNextID(b *testing.B) {
	sf, err := New(Settings{StartTime: time.Now().Add(-time.Hour), MachineID: fixedMachine(1)})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sf.NextID(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSinflakeNextIDParallel(b *testing.B) {
	sf, err := New(Settings{StartTime: time.Now().Add(-time.Hour), MachineID: fixedMachine(1)})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := sf.NextID(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkSinflakeNextIDs16(b *testing.B) {
	sf, err := New(Settings{StartTime: time.Now().Add(-time.Hour), MachineID: fixedMachine(1)})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sf.NextIDs(16); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSegmentNextID(b *testing.B) {
	ctx := context.Background()
	g, err := NewSegmentGenerator(newRecStore(100000))
	if err != nil {
		b.Fatal(err)
	}
	if err := g.Init(ctx); err != nil {
		b.Fatal(err)
	}
	defer g.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := g.NextID(ctx, "order"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSegmentNextIDParallel(b *testing.B) {
	ctx := context.Background()
	g, err := NewSegmentGenerator(newRecStore(100000))
	if err != nil {
		b.Fatal(err)
	}
	if err := g.Init(ctx); err != nil {
		b.Fatal(err)
	}
	defer g.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := g.NextID(ctx, "order"); err != nil {
				b.Fatal(err)
			}
		}
	})
}
