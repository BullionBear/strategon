package cron

import (
	"testing"
	"time"
)

func TestParseRequiresTimezone(t *testing.T) {
	if _, err := Parse("0 0 * * *", ""); err == nil {
		t.Fatal("expected error for empty timezone")
	}
	if _, err := Parse("0 0 * * *", "Not/AZone"); err == nil {
		t.Fatal("expected error for bad timezone")
	}
	if _, err := Parse("not a cron", "UTC"); err == nil {
		t.Fatal("expected error for bad expr")
	}
}

func TestNextAfterUTCMidnight(t *testing.T) {
	s, err := Parse("0 0 * * *", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	next := s.NextAfter(after, 0, nil)
	want := time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

func TestNextAfterJitterDeterministic(t *testing.T) {
	s, err := Parse("0 0 * * *", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	next := s.NextAfter(after, 30, func(n int32) int32 { return 15 })
	want := time.Date(2024, 6, 2, 0, 0, 15, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

func TestNextAfterAsiaTaipei(t *testing.T) {
	s, err := Parse("0 0 * * *", "Asia/Taipei")
	if err != nil {
		t.Fatal(err)
	}
	// 2024-06-01 12:00 UTC = 20:00 Taipei → next local midnight is 2024-06-02 00:00 CST = 2024-06-01 16:00 UTC
	after := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	next := s.NextAfter(after, 0, nil)
	want := time.Date(2024, 6, 1, 16, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %v (%s), want %v", next, next.In(time.FixedZone("CST", 8*3600)), want)
	}
}
