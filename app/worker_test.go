package main

import (
	"reflect"
	"testing"
	"time"
)

func date(y, m, d int) time.Time {
	return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
}

func monthPtr(t time.Time) *time.Time { return &t }

func TestGetMonthsToCollect(t *testing.T) {
	tests := []struct {
		name      string
		point     MeteringPoint
		refTime   time.Time
		fetchFrom *time.Time
		want      []monthYear
	}{
		{
			name:    "first collection without cap backfills from available_from",
			point:   MeteringPoint{AvailableFrom: monthPtr(date(2021, 1, 1))},
			refTime: date(2021, 3, 15),
			want:    []monthYear{{1, 2021}, {2, 2021}, {3, 2021}},
		},
		{
			name:      "cap raises the first-time start",
			point:     MeteringPoint{AvailableFrom: monthPtr(date(2021, 1, 1))},
			refTime:   date(2025, 9, 15),
			fetchFrom: monthPtr(date(2025, 7, 1)),
			want:      []monthYear{{7, 2025}, {8, 2025}, {9, 2025}},
		},
		{
			name:      "cap earlier than available_from is ignored",
			point:     MeteringPoint{AvailableFrom: monthPtr(date(2025, 3, 1))},
			refTime:   date(2025, 5, 15),
			fetchFrom: monthPtr(date(2025, 1, 1)),
			want:      []monthYear{{3, 2025}, {4, 2025}, {5, 2025}},
		},
		{
			name:    "subsequent collection refetches from the last month",
			point:   MeteringPoint{LastMeterReadingCollection: monthPtr(date(2025, 6, 10))},
			refTime: date(2025, 9, 15),
			want:    []monthYear{{6, 2025}, {7, 2025}, {8, 2025}, {9, 2025}},
		},
		{
			name:      "cap applies to subsequent collection too",
			point:     MeteringPoint{LastMeterReadingCollection: monthPtr(date(2025, 6, 10))},
			refTime:   date(2025, 9, 15),
			fetchFrom: monthPtr(date(2025, 7, 1)),
			want:      []monthYear{{7, 2025}, {8, 2025}, {9, 2025}},
		},
		{
			name:    "last collection in the reference month yields only that month",
			point:   MeteringPoint{LastMeterReadingCollection: monthPtr(date(2025, 9, 1))},
			refTime: date(2025, 9, 15),
			want:    []monthYear{{9, 2025}},
		},
		{
			name:      "cap in the future fetches nothing",
			point:     MeteringPoint{AvailableFrom: monthPtr(date(2021, 1, 1))},
			refTime:   date(2025, 9, 15),
			fetchFrom: monthPtr(date(2026, 1, 1)),
			want:      nil,
		},
		{
			name:    "no available_from falls back to the reference month",
			point:   MeteringPoint{},
			refTime: date(2025, 9, 15),
			want:    []monthYear{{9, 2025}},
		},
		{
			name:    "backfill crosses a year boundary",
			point:   MeteringPoint{AvailableFrom: monthPtr(date(2024, 11, 1))},
			refTime: date(2025, 2, 15),
			want:    []monthYear{{11, 2024}, {12, 2024}, {1, 2025}, {2, 2025}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getMonthsToCollect(tt.point, tt.refTime, tt.fetchFrom)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("getMonthsToCollect() = %v, want %v", got, tt.want)
			}
		})
	}
}
