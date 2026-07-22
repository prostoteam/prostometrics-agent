package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

type testCollector struct {
	collect func(context.Context) error
}

func (testCollector) ID() string           { return "test" }
func (testCollector) Every() time.Duration { return time.Second }
func (c testCollector) Collect(ctx context.Context) error {
	return c.collect(ctx)
}

func TestSafeCollectReturnsErrorsAndRecoversPanics(t *testing.T) {
	want := errors.New("collect failed")
	if got := safeCollect(context.Background(), testCollector{collect: func(context.Context) error { return want }}); !errors.Is(got, want) {
		t.Fatalf("safeCollect error = %v", got)
	}
	got := safeCollect(context.Background(), testCollector{collect: func(context.Context) error { panic("boom") }})
	if got == nil || got.Error() != "panic: boom" {
		t.Fatalf("safeCollect panic error = %v", got)
	}
}
