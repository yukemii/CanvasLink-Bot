package canvas

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/markadodo/canvaslink/internal/store"
)

// Event represents a parsed Canvas calendar event.
type Event struct {
	UID         string
	Title       string
	Description string
	Location    string
	URL         string
	DueAt       *time.Time
	AllDay      bool
	Course      string
	CourseName  string
	Type        string
	DTStamp     *time.Time
	Sequence    int
}

// Default course code regex — matches NUS-style codes like CP2106, CS2040S, CS2103T, IS1103.
// Can be overridden via CANVASLINK_COURSE_REGEX env var for other universities.
var courseCodePattern = regexp.MustCompile(`(?i)(?:\[|\b)([A-Z]{2,4}\d{3,4}[A-Z]?)(?:\]|\b)`)

// bracketCodePattern specifically extracts course codes from [CODE] at end of SUMMARY.
var bracketCodePattern = regexp.MustCompile(`(?i)\[([A-Z]{2,4}\d{3,4}[A-Z]?)\]\s*$`)

// FetchAndDetect fetches an iCal feed and returns parsed events and course seeds.
func FetchAndDetect(url string) ([]Event, []store.CourseTypeSeed, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("canvas feed returned status %d", resp.StatusCode)
	}

	events, err := parseICS(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	// Build unique course seeds from detected events
	seedMap := map[string]store.CourseTypeSeed{}
	for _, ev := range events {
		if ev.Course == "" {
			continue
		}
		key := ev.Course + "|" + ev.Type
		if _, ok := seedMap[key]; ok {
			continue
		}
		courseName := ev.CourseName
		if courseName == "" {
			courseName = ev.Course
		}
		seedMap[key] = store.CourseTypeSeed{
			CourseID:       ev.Course,
			CourseName:     courseName,
			AssignmentType: ev.Type,
		}
	}

	seeds := make([]store.CourseTypeSeed, 0, len(seedMap))
	for _, row := range seedMap {
		seeds = append(seeds, row)
	}

	return events, seeds, nil
}

// parseICS parses an iCal feed from a reader.
func parseICS(r io.Reader) ([]Event, error) {
	lines, err := unfoldLines(r)
	if err != nil {
		return nil, err
	}

	var events []Event
	inEvent := false
	current := map[string]string{}

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "BEGIN:VEVENT":
			inEvent = true
			current = map[string]string{}
			continue
		case line == "END:VEVENT":
			if inEvent {
				ev := toEvent(current)
				if !isNonCourseEvent(ev) {
					events = append(events, ev)
				}
			}
			inEvent = false
			current = map[string]string{}
			continue
		}
		if !inEvent {
			continue
		}

		// Parse iCal property line: KEY;PARAM1=VALUE1;PARAM2=VALUE2:VALUE
		// The first colon separates the property name from its value.
		// But URLs contain colons, so we need to be careful.
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		propName := strings.ToUpper(strings.TrimSpace(line[:idx]))
		propValue := strings.TrimSpace(line[idx+1:])

		// Extract the base property name (before any semicolons)
		baseName := propName
		if semiIdx := strings.Index(propName, ";"); semiIdx > 0 {
			baseName = propName[:semiIdx]
		}

		// Store under both the full property name and the base name
		current[propName] = propValue
		if baseName != propName {
			// Only set base name if not already set (first occurrence wins)
			if _, exists := current[baseName]; !exists {
				current[baseName] = propValue
			}
		}
	}

	return events, nil
}

// toEvent converts a raw iCal property map to an Event struct.
func toEvent(raw map[string]string) Event {
	title := strings.TrimSpace(raw["SUMMARY"])
	uid := strings.TrimSpace(raw["UID"])
	categories := strings.TrimSpace(raw["CATEGORIES"])
	description := strings.TrimSpace(raw["DESCRIPTION"])
	location := strings.TrimSpace(raw["LOCATION"])
	icalURL := strings.TrimSpace(raw["URL"])

	// Parse DTSTAMP for change detection
	var dtStamp *time.Time
	if ds := strings.TrimSpace(raw["DTSTAMP"]); ds != "" {
		dtStamp = parseDate(ds)
	}

	// Parse SEQUENCE for change detection
	sequence := 0
	if seqStr := strings.TrimSpace(raw["SEQUENCE"]); seqStr != "" {
		fmt.Sscanf(seqStr, "%d", &sequence)
	}

	due := parseDate(raw["DTSTART"], raw["DTEND"])

	// Detect all-day events (VALUE=DATE format without time)
	_, isAllDay := raw["DTSTART;VALUE=DATE"]
	if !isAllDay {
		_, isAllDay = raw["DTSTART;VALUE=DATE;VALUE=DATE"]
	}

	courseID, courseName := detectCourse(title, categories, description, icalURL)
	assignmentType := detectType(title)

	return Event{
		UID:         uid,
		Title:       title,
		Description: description,
		Location:    location,
		URL:         icalURL,
		DueAt:       due,
		AllDay:      isAllDay,
		Course:      courseID,
		CourseName:  courseName,
		Type:        assignmentType,
		DTStamp:     dtStamp,
		Sequence:    sequence,
	}
}

// isNonCourseEvent returns true if the event has no course association
// (public holidays, non-academic events, etc.).
func isNonCourseEvent(ev Event) bool {
	if ev.Course == "" {
		return true // Filter anything with no course association
	}
	return false
}

// detectCourse extracts the course ID and full course name from iCal fields.
// Priority order:
//  1. CATEGORIES field (assignment feed)
//  2. DESCRIPTION field (assignment feed — "Course: CS2040S Data Structures...")
//  3. SUMMARY bracket code (calendar feed — "[CP2106]" at end of title)
//  4. SUMMARY regex fallback (any course code pattern in title)
func detectCourse(title, categories, description, icalURL string) (courseID, courseName string) {
	// Priority 1: CATEGORIES field
	if categories != "" {
		first := strings.Split(categories, ",")[0]
		first = strings.TrimSpace(first)
		if first != "" {
			courseID = first
		}
	}

	// Priority 2: DESCRIPTION field
	desc := strings.TrimSpace(description)
	if desc != "" && courseID == "" {
		if cm := courseCodePattern.FindStringSubmatch(desc); len(cm) > 1 {
			courseID = strings.ToUpper(cm[1])
		}
	}

	// Priority 3: SUMMARY bracket code — calendar feed puts [CP2106] at end of title
	titleUpper := strings.TrimSpace(title)
	if courseID == "" && titleUpper != "" {
		if m := bracketCodePattern.FindStringSubmatch(titleUpper); len(m) > 1 {
			courseID = strings.ToUpper(m[1])
		}
	}

	// Priority 4: SUMMARY regex fallback
	if courseID == "" && titleUpper != "" {
		if m := courseCodePattern.FindStringSubmatch(titleUpper); len(m) > 1 {
			courseID = strings.ToUpper(m[1])
		}
	}

	// Priority 5: URL field
	if courseID == "" && icalURL != "" {
		if m := courseCodePattern.FindStringSubmatch(icalURL); len(m) > 1 {
			courseID = strings.ToUpper(m[1])
		}
	}

	// Use course ID as name if we have one
	if courseID != "" {
		courseName = courseID
	}

	return courseID, courseName
}

// detectType classifies an event by its title keywords.
func detectType(title string) string {
	s := strings.ToLower(title)
	switch {
	case strings.Contains(s, "quiz"):
		return "quiz"
	case strings.Contains(s, "exam"):
		return "exam"
	case strings.Contains(s, "test"):
		return "exam"
	case strings.Contains(s, "midterm"):
		return "exam"
	case strings.Contains(s, "final"):
		return "exam"
	case strings.Contains(s, "lab"):
		return "assignment"
	case strings.Contains(s, "assignment"):
		return "assignment"
	case strings.Contains(s, "project"):
		return "assignment"
	case strings.Contains(s, "homework"):
		return "assignment"
	case strings.Contains(s, "problem set"):
		return "assignment"
	case strings.Contains(s, "tutorial"):
		return "assignment"
	case strings.Contains(s, "live qna"):
		return "assignment"
	case strings.Contains(s, "live q&a"):
		return "assignment"
	case strings.Contains(s, "recitation"):
		return "assignment"
	default:
		return "assignment"
	}
}

// parseDate tries to parse a date string using multiple iCal formats.
func parseDate(candidates ...string) *time.Time {
	layouts := []string{
		"20060102T150405Z",
		"20060102T150405",
		"20060102",
	}
	for _, raw := range candidates {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		for _, layout := range layouts {
			t, err := time.Parse(layout, v)
			if err == nil {
				return &t
			}
		}
	}
	return nil
}

// Init allows configuration of the canvas parser, such as a custom course code regex.
// Call this once at startup before using FetchAndDetect.
func Init(customCourseRegex string) {
	if customCourseRegex != "" {
		compiled, err := regexp.Compile(customCourseRegex)
		if err != nil {
			fmt.Printf("canvas: invalid CANVASLINK_COURSE_REGEX=%q, using default: %v\n", customCourseRegex, err)
			return
		}
		courseCodePattern = compiled
		// Also update the bracket code pattern to use the same inner pattern
		bracketCodePattern = regexp.MustCompile(`(?i)\[(` + compiled.String() + `)\]\s*$`)
	}
}

// unfoldLines handles iCal line continuation (RFC 5545).
// Lines starting with a space or tab are continuations of the previous line.
func unfoldLines(r io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(r)
	// Increase buffer for long lines (e.g., X-ALT-DESC with embedded HTML)
	scanner.Buffer(make([]byte, 0, 1024*64), 1024*256)
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if len(lines) == 0 {
			lines = append(lines, line)
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			lines[len(lines)-1] += strings.TrimLeft(line, " \t")
			continue
		}
		lines = append(lines, line)
	}
	return lines, scanner.Err()
}
