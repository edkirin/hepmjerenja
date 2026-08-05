package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type openMeteoResponse struct {
	Daily struct {
		Time               []string  `json:"time"`
		Weathercode        []int     `json:"weathercode"`
		ShortwaveRadiation []float64 `json:"shortwave_radiation_sum"`
		Sunrise            []string  `json:"sunrise"`             // "YYYY-MM-DDTHH:MM"
		Sunset             []string  `json:"sunset"`              // "YYYY-MM-DDTHH:MM"
		TempMax            []float64 `json:"temperature_2m_max"`
		TempMin            []float64 `json:"temperature_2m_min"`
	} `json:"daily"`
}

// FetchInsolation retrieves daily weather codes and shortwave radiation sums
// for the given coordinates and calendar month from the Open-Meteo archive API.
// The end date is capped at yesterday to stay within archive coverage.
// Days without data (e.g. the tail of the current month) are omitted.
func FetchInsolation(ctx context.Context, lat, lon float64, year, month int) ([]InsolationDay, error) {
	start := fmt.Sprintf("%d-%02d-01", year, month)

	lastOfMonth := time.Date(year, time.Month(month+1), 0, 0, 0, 0, 0, time.UTC)
	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	endDate := lastOfMonth
	if yesterday.Before(endDate) {
		endDate = yesterday
	}

	if endDate.Before(time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)) {
		return nil, nil // entire month is still in the future
	}

	q := url.Values{}
	q.Set("latitude", fmt.Sprintf("%.6f", lat))
	q.Set("longitude", fmt.Sprintf("%.6f", lon))
	q.Set("start_date", start)
	q.Set("end_date", endDate.Format("2006-01-02"))
	q.Set("daily", "weathercode,shortwave_radiation_sum,sunrise,sunset,temperature_2m_max,temperature_2m_min")
	q.Set("timezone", "Europe/Zagreb")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://archive-api.open-meteo.com/v1/archive?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open-meteo request: %w", err)
	}
	defer resp.Body.Close()

	var omr openMeteoResponse
	if err := json.NewDecoder(resp.Body).Decode(&omr); err != nil {
		return nil, fmt.Errorf("open-meteo decode: %w", err)
	}

	// hhmm extracts "HH:MM" from an ISO8601 datetime "YYYY-MM-DDTHH:MM".
	hhmm := func(s string) string {
		if len(s) >= 16 {
			return s[11:16]
		}
		return ""
	}

	days := make([]InsolationDay, 0, len(omr.Daily.Time))
	for i, dateStr := range omr.Daily.Time {
		t, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}
		d := InsolationDay{Day: t.Day()}
		if i < len(omr.Daily.Weathercode) {
			d.Weathercode = omr.Daily.Weathercode[i]
		}
		if i < len(omr.Daily.ShortwaveRadiation) {
			d.Radiation = omr.Daily.ShortwaveRadiation[i]
		}
		if i < len(omr.Daily.Sunrise) {
			d.Sunrise = hhmm(omr.Daily.Sunrise[i])
		}
		if i < len(omr.Daily.Sunset) {
			d.Sunset = hhmm(omr.Daily.Sunset[i])
		}
		if i < len(omr.Daily.TempMax) {
			d.TempMax = omr.Daily.TempMax[i]
		}
		if i < len(omr.Daily.TempMin) {
			d.TempMin = omr.Daily.TempMin[i]
		}
		days = append(days, d)
	}
	return days, nil
}
