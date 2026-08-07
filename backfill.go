package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// Backfill exists because One Call 4.0's daily timeline reaches back decades on
// the same endpoint the forecast comes from, so the dashboard does not have to
// start empty and grow: a year of real daily maxima and minima can be loaded on
// day one. The cost is calls, not time -- one page is daysPerPage days, so a
// year is about 37 calls out of the 1,000-a-day free allowance.
const (
	// Pages per invocation, chosen so a single accidental backfill cannot spend
	// the whole daily allowance. 60 pages is roughly 600 days.
	defaultBackfillMaxPages = 60
	// Left on the clock when deciding whether to fetch another page, so a run
	// that hits the limit stops between pages with its work saved instead of
	// being killed part-way through writing a year object.
	backfillTimeReserve = 45 * time.Second
	backfillPace        = 200 * time.Millisecond
)

// backfill walks the daily timeline forward from backfill_from, folding each
// page into daily/YYYY.json. It stops at backfill_to, at the page limit, or
// before the invocation runs out of time -- whichever comes first -- and
// reports how far it got, so a long range is resumed by invoking again from the
// day after completed_through.
func (a *app) backfill(ctx context.Context, weather *openWeather, event collectEvent) (any, error) {
	from, err := time.ParseInLocation("2006-01-02", event.BackfillFrom, a.zone)
	if err != nil {
		return nil, fmt.Errorf("backfill_from must be YYYY-MM-DD: %w", err)
	}

	now := a.now()
	to := startOfDay(now, a.zone)
	if event.BackfillTo != "" {
		to, err = time.ParseInLocation("2006-01-02", event.BackfillTo, a.zone)
		if err != nil {
			return nil, fmt.Errorf("backfill_to must be YYYY-MM-DD: %w", err)
		}
	}
	if to.Before(from) {
		return nil, fmt.Errorf("backfill_to %s is before backfill_from %s", to.Format("2006-01-02"), from.Format("2006-01-02"))
	}

	maxPages := defaultBackfillMaxPages
	deadline, hasDeadline := ctx.Deadline()

	cursor := from
	pages := 0
	archived := 0
	completed := ""

	for !cursor.After(to) && pages < maxPages {
		if hasDeadline && time.Until(deadline) < backfillTimeReserve {
			log.Printf("backfill stopping early at %s to stay inside the invocation timeout", cursor.Format("2006-01-02"))
			break
		}

		page, err := weather.days(ctx, cursor.Unix())
		if err != nil {
			// Whatever has already been written stays written; the caller
			// resumes from completed_through rather than starting over.
			return backfillResult(from, to, completed, pages, archived), fmt.Errorf("read daily timeline at %s: %w", cursor.Format("2006-01-02"), err)
		}
		pages++

		days := buildDays(page.Data, a.zone, now)
		// The last page of a range overshoots backfill_to by up to
		// daysPerPage-1 days, and past today it is forecast rather than
		// history. Trimming here keeps a backfill from writing forecasts the
		// scheduled run would have to correct.
		days = trimTo(days, to)
		if len(days) == 0 {
			break
		}
		if err := a.archiveDays(ctx, days); err != nil {
			return backfillResult(from, to, completed, pages, archived), fmt.Errorf("archive backfilled days: %w", err)
		}
		archived += len(days)
		completed = days[len(days)-1].Date

		cursor = cursor.AddDate(0, 0, daysPerPage)
		time.Sleep(backfillPace)
	}

	return backfillResult(from, to, completed, pages, archived), nil
}

func trimTo(days []dayRecord, to time.Time) []dayRecord {
	limit := to.Format("2006-01-02")
	trimmed := make([]dayRecord, 0, len(days))
	for _, day := range days {
		if day.Date > limit {
			continue
		}
		trimmed = append(trimmed, day)
	}
	return trimmed
}

func backfillResult(from, to time.Time, completed string, pages, archived int) map[string]any {
	return map[string]any{
		"backfill_from":     from.Format("2006-01-02"),
		"backfill_to":       to.Format("2006-01-02"),
		"completed_through": completed,
		"pages_fetched":     pages,
		"days_archived":     archived,
	}
}
