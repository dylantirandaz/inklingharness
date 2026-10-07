package migration_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"fixture/catalog"
	"fixture/report"
	"fixture/service"
)

func TestConsumersKeepValuesOrderAndMessages(t *testing.T) {
	s := catalog.New(map[string]string{"a": "one", "empty": ""})
	ctx := context.Background()
	values, err := service.ResolveAll(ctx, s, []string{"empty", "a", "a"})
	if err != nil || !reflect.DeepEqual(values, []string{"", "one", "one"}) { t.Fatalf("batch: %v, %v", values, err) }
	line, err := report.Line(ctx, s, "a")
	if err != nil || line != "a=one\n" { t.Fatalf("line: %q, %v", line, err) }
	value, err := report.Optional(ctx, s, "empty", "fallback")
	if err != nil || value != "" { t.Fatalf("empty: %q, %v", value, err) }
	_, err = report.Line(ctx, s, "absent")
	want := `report: resolve "absent": read "absent": missing catalog key`
	if err == nil || err.Error() != want { t.Fatalf("message: %v, want %s", err, want) }
	value, err = report.Optional(ctx, s, "absent", "fallback")
	if err != nil || value != "fallback" { t.Fatalf("optional: %q, %v", value, err) }
}

func TestErrorIdentityAcrossEveryConsumer(t *testing.T) {
	calls := []struct {
		name string
		call func(context.Context, *catalog.Store) error
	}{
		{"catalog", func(ctx context.Context, s *catalog.Store) error { _, err := s.Lookup(ctx, "absent"); return err }},
		{"service", func(ctx context.Context, s *catalog.Store) error { _, err := service.Resolve(ctx, s, "absent"); return err }},
		{"batch", func(ctx context.Context, s *catalog.Store) error {
			values, err := service.ResolveAll(ctx, s, []string{"ok", "absent"})
			if values != nil { t.Errorf("partial batch leaked: %v", values) }
			return err
		}},
		{"report", func(ctx context.Context, s *catalog.Store) error { _, err := report.Line(ctx, s, "absent"); return err }},
	}
	for _, call := range calls {
		t.Run(call.name, func(t *testing.T) {
			s := catalog.New(map[string]string{"ok": "yes"})
			if err := call.call(context.Background(), s); !errors.Is(err, catalog.ErrMissing) { t.Fatalf("missing identity: %v", err) }
			s.Close()
			if err := call.call(context.Background(), s); !errors.Is(err, catalog.ErrClosed) { t.Fatalf("closed identity: %v", err) }
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := call.call(ctx, s); !errors.Is(err, context.Canceled) { t.Fatalf("cancel must take precedence: %v", err) }
		})
	}
}

func TestCancellationAndDeadlineOnPresentValues(t *testing.T) {
	s := catalog.New(map[string]string{"ok": "yes"})
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	_, err := report.Optional(ctx, s, "ok", "fallback")
	if !errors.Is(err, context.DeadlineExceeded) { t.Fatalf("deadline: %v", err) }
	_, err = service.Resolve(ctx, s, "ok")
	if !errors.Is(err, context.DeadlineExceeded) { t.Fatalf("service deadline: %v", err) }
	s.Close()
	_, err = report.Optional(context.Background(), s, "absent", "fallback")
	if !errors.Is(err, catalog.ErrClosed) || !strings.Contains(err.Error(), "optional") { t.Fatalf("closed optional: %v", err) }
}
