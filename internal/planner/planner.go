// Package planner contains timezone-aware rules shared by chat and background delivery.
package planner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/markadodo/canvaslink/internal/store"
)

func Location(zone string) *time.Location {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.UTC
	}
	return loc
}
func Quiet(p store.PlannerPreferences, now time.Time) bool {
	if p.QuietStart == p.QuietEnd {
		return false
	}
	h := now.Hour()
	if p.QuietStart < p.QuietEnd {
		return h >= p.QuietStart && h < p.QuietEnd
	}
	return h >= p.QuietStart || h < p.QuietEnd
}
func Bounds(view string, now time.Time) (time.Time, time.Time) {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch view {
	case "today":
		return start, start.AddDate(0, 0, 1)
	case "week":
		return start, start.AddDate(0, 0, 7)
	}
	return start, time.Time{}
}
func Select(tasks []store.PlannerTask, view, course string, now time.Time) []store.PlannerTask {
	start, end := Bounds(view, now)
	out := []store.PlannerTask{}
	for _, t := range tasks {
		if !t.Present || (course != "" && t.CourseID != course) {
			continue
		}
		if view == "done" {
			if t.Done {
				out = append(out, t)
			}
			continue
		}
		if t.Done {
			continue
		}
		due := t.EffectiveDue(now.Location())
		if due.Before(start) {
			continue
		}
		if !end.IsZero() && !due.Before(end) {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].EffectiveDue(now.Location()).Before(out[j].EffectiveDue(now.Location()))
	})
	return out
}
func Summary(title string, tasks []store.PlannerTask, loc *time.Location, limit int) string {
	var b strings.Builder
	b.WriteString(title + "\n")
	if len(tasks) == 0 {
		b.WriteString("No outstanding items in this view.")
		return b.String()
	}
	for i, t := range tasks {
		if i >= limit {
			fmt.Fprintf(&b, "\n…and %d more. Open /upcoming.", len(tasks)-limit)
			break
		}
		course := t.CourseID
		if t.Manual {
			course = "Personal"
		}
		fmt.Fprintf(&b, "\n• %s · %s\n  %s\n", Short(t.Title, 100), course, store.PlannerDate(t.DueAt, t.AllDay, loc))
		if t.TargetAt != nil {
			fmt.Fprintf(&b, "  Personal target: %s\n", store.PlannerDate(*t.TargetAt, false, loc))
		}
	}
	return b.String()
}
func Short(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
func ParseOffsets(text string) ([]int, error) {
	if strings.EqualFold(strings.TrimSpace(text), "off") {
		return []int{}, nil
	}
	seen := map[int]bool{}
	var out []int
	for _, part := range strings.Split(text, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if len(p) < 2 {
			return nil, fmt.Errorf("use offsets such as 1h, 1d, 1w")
		}
		n, err := strconv.Atoi(p[:len(p)-1])
		unit := map[byte]int{'m': 1, 'h': 60, 'd': 1440, 'w': 10080}[p[len(p)-1]]
		if err != nil || n <= 0 || unit == 0 || n > 43200/unit {
			return nil, fmt.Errorf("offsets must be between 1 minute and 30 days")
		}
		v := n * unit
		if !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	if len(out) > 5 {
		return nil, fmt.Errorf("choose at most 5 reminder offsets")
	}
	sort.Ints(out)
	return out, nil
}
func OffsetLabel(offsets []int) string {
	if len(offsets) == 0 {
		return "Off"
	}
	var out []string
	for _, v := range offsets {
		unit := "m"
		n := v
		if v%10080 == 0 {
			n = v / 10080
			unit = "w"
		} else if v%1440 == 0 {
			n = v / 1440
			unit = "d"
		} else if v%60 == 0 {
			n = v / 60
			unit = "h"
		}
		out = append(out, fmt.Sprintf("%d%s", n, unit))
	}
	return strings.Join(out, ", ") + " before"
}
func ParseClock(s string) bool { _, err := time.Parse("15:04", s); return err == nil && len(s) == 5 }
func DigestDue(p store.PlannerPreferences, weekly bool, now time.Time) (bool, string) {
	enabled, clock := p.Daily, p.DailyTime
	kind := "daily"
	if weekly {
		enabled, clock, kind = p.Weekly, p.WeeklyTime, "weekly"
		if int(now.Weekday()) != p.Weekday {
			return false, ""
		}
	}
	if !enabled || !ParseClock(clock) {
		return false, ""
	}
	parsed, _ := time.Parse("15:04", clock)
	at := time.Date(now.Year(), now.Month(), now.Day(), parsed.Hour(), parsed.Minute(), 0, 0, now.Location())
	// Catch up on the same local day only; never flood users with old digests.
	return !now.Before(at), kind + ":" + now.Format(time.DateOnly)
}
