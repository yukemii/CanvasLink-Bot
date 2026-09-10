package google

import (
	"context"
	"fmt"
	"strings"

	googlecalendar "google.golang.org/api/calendar/v3"
)

type CalendarChoice struct{ ID, Name string }

func (c *CalendarClient) WritableCalendars(ctx context.Context, user int64) ([]CalendarChoice, error) {
	ctx, cancel := context.WithTimeout(ctx, calendarOperationTimeout)
	defer cancel()
	service, err := c.serviceForUser(ctx, user)
	if err != nil {
		return nil, err
	}
	entries, err := listWritable(ctx, service)
	if err != nil {
		return nil, wrapEventOperationError("list writable calendars", err)
	}
	out := []CalendarChoice{}
	for _, e := range entries {
		out = append(out, CalendarChoice{e.Id, e.Summary})
	}
	return out, nil
}
func listWritable(ctx context.Context, service *googlecalendar.Service) ([]*googlecalendar.CalendarListEntry, error) {
	var out []*googlecalendar.CalendarListEntry
	token := ""
	for {
		call := service.CalendarList.List().MinAccessRole("writer").MaxResults(250)
		if token != "" {
			call.PageToken(token)
		}
		page, err := call.Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		token = page.NextPageToken
		if token == "" {
			return out, nil
		}
	}
}

// EnsureDestination is called while holding the user's feed lock. The private
// description marker lets a retry discover a calendar created before a crash.
func (c *CalendarClient) EnsureDestination(ctx context.Context, user int64) (string, error) {
	if c.store == nil {
		return "primary", nil
	} // Store-free clients are used by HTTP adapter tests.
	p, err := c.store.PlannerPreferences(ctx, user)
	if err != nil {
		return "", err
	}
	if p.CalendarID != "" {
		return p.CalendarID, nil
	}
	opCtx, cancel := context.WithTimeout(ctx, calendarOperationTimeout)
	defer cancel()
	service, err := c.serviceForUser(opCtx, user)
	if err != nil {
		return "", err
	}
	marker := fmt.Sprintf("CanvasLink managed calendar · %s", sourceUIDHash(fmt.Sprintf("%s:%d", c.instanceID, user)))
	calendars, err := listWritable(opCtx, service)
	if err != nil {
		return "", wrapEventOperationError("find CanvasLink calendar", err)
	}
	for _, entry := range calendars {
		if entry.AccessRole == "owner" && !entry.Primary && entry.Description == marker {
			p.CalendarID = entry.Id
			p.CalendarName = entry.Summary
			break
		}
	}
	if p.CalendarID == "" {
		created, err := service.Calendars.Insert(&googlecalendar.Calendar{Summary: "CanvasLink", Description: marker}).Context(opCtx).Do()
		if err != nil {
			return "", wrapEventOperationError("create CanvasLink calendar", err)
		}
		if created == nil || created.Id == "" {
			return "", fmt.Errorf("Google returned no calendar ID")
		}
		p.CalendarID = created.Id
		p.CalendarName = "CanvasLink"
	}
	if err := c.store.SavePlannerPreferences(ctx, user, p); err != nil {
		return "", err
	}
	return p.CalendarID, nil
}
func (c *CalendarClient) styledEvent(ctx context.Context, user int64, source, title string, dueEvent *googlecalendar.Event, description string) (*googlecalendar.Event, error) {
	if c.store == nil {
		return dueEvent, nil
	}
	p, err := c.store.PlannerPreferences(ctx, user)
	if err != nil {
		return nil, err
	}
	course := ""
	for _, line := range strings.Split(description, "\n") {
		if strings.HasPrefix(line, "Course: ") {
			course = strings.TrimPrefix(line, "Course: ")
			break
		}
	}
	if p.TitleStyle == "course" && course != "" && !strings.HasPrefix(title, "["+course+"]") {
		dueEvent.Summary = "[" + course + "] " + title
	}
	if color := p.Colors[course]; color != "" {
		dueEvent.ColorId = color
	} else {
		dueEvent.ColorId = ""
		dueEvent.ForceSendFields = append(dueEvent.ForceSendFields, "ColorId")
	}
	return dueEvent, nil
}

// CheckConnection detects revoked grants even when there are no new assignments.
func (c *CalendarClient) CheckConnection(ctx context.Context, user int64) error {
	opCtx, cancel := context.WithTimeout(ctx, calendarOperationTimeout)
	defer cancel()
	service, err := c.serviceForUser(opCtx, user)
	if err != nil {
		return err
	}
	_, err = service.CalendarList.List().MaxResults(1).Context(opCtx).Do()
	if err != nil {
		return wrapEventOperationError("check Google Calendar connection", err)
	}
	return nil
}
