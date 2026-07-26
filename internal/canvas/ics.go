package canvas

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

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
	Status      string
	Cancelled   bool
}

// Default course code regex — matches NUS-style codes like CP2106, CS2040S, CS2103T, IS1103.
// Can be overridden via CANVASLINK_COURSE_REGEX env var for other universities.
var courseCodePattern = regexp.MustCompile(`(?i)(?:\[|\b)([A-Z]{2,4}\d{3,4}[A-Z]?)(?:\]|\b)`)

// bracketCodePattern specifically extracts course codes from [CODE] at end of SUMMARY.
var bracketCodePattern = regexp.MustCompile(`(?i)\[([A-Z]{2,4}\d{3,4}[A-Z]?)\]\s*$`)

const (
	maxFeedSize            = 5 << 20
	maxCalendarLines       = 100_000
	maxCalendarEvents      = 10_000
	maxEventProperties     = 512
	maxPropertyHeaderSize  = 4 << 10
	maxPropertyValueSize   = 256 << 10
	maxPhysicalLineSize    = maxPropertyHeaderSize + maxPropertyValueSize
	maxUIDSize             = 2 << 10
	maxActionableTitleSize = 1024
	maxDetectedCourseSize  = 256
	fetchTimeout           = 20 * time.Second
)

// FeedSecurityOptions controls which Canvas feed destinations may be reached.
// The zero value is the secure production policy: HTTPS and publicly routable
// destinations only. Development environments can explicitly relax either
// restriction.
type FeedSecurityOptions struct {
	AllowHTTP            bool
	AllowPrivateNetworks bool
}

type feedClientConfig struct {
	client  *http.Client
	options FeedSecurityOptions
}

var configuredFeedClient atomic.Pointer[feedClientConfig]

func init() {
	configuredFeedClient.Store(newFeedClientConfig(FeedSecurityOptions{}))
}

// ConfigureFeedSecurity replaces the process-wide Canvas feed transport.
// Call it during startup, before starting the bot or sync worker. The zero
// value restores the secure default policy.
func ConfigureFeedSecurity(options FeedSecurityOptions) {
	previous := configuredFeedClient.Swap(newFeedClientConfig(options))
	if previous != nil && previous.client != nil {
		previous.client.CloseIdleConnections()
	}
}

// FetchAndDetect fetches an iCal feed and returns parsed events and course seeds.
func FetchAndDetect(rawURL string) ([]Event, []store.CourseTypeSeed, error) {
	return FetchAndDetectContext(context.Background(), rawURL)
}

// FetchAndDetectContext is FetchAndDetect with caller-controlled cancellation.
func FetchAndDetectContext(ctx context.Context, rawURL string) ([]Event, []store.CourseTypeSeed, error) {
	config := configuredFeedClient.Load()
	if config == nil {
		return nil, nil, errors.New("canvas feed HTTP client is not configured")
	}
	return fetchAndDetectWithOptions(ctx, config.client, rawURL, config.options)
}

func fetchAndDetect(ctx context.Context, client *http.Client, rawURL string) ([]Event, []store.CourseTypeSeed, error) {
	return fetchAndDetectWithOptions(ctx, client, rawURL, FeedSecurityOptions{})
}

func fetchAndDetectWithOptions(
	ctx context.Context,
	client *http.Client,
	rawURL string,
	options FeedSecurityOptions,
) ([]Event, []store.CourseTypeSeed, error) {
	if ctx == nil {
		return nil, nil, errors.New("canvas feed context is nil")
	}
	if client == nil {
		return nil, nil, errors.New("canvas feed HTTP client is nil")
	}

	parsedURL, err := url.ParseRequestURI(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, nil, errors.New("invalid canvas feed URL")
	}
	if err := validateFeedURL(parsedURL, options); err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return nil, nil, errors.New("could not create canvas feed request")
	}
	req.Header.Set("Accept", "text/calendar, application/ics, text/plain;q=0.9, */*;q=0.1")

	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return nil, nil, fmt.Errorf("fetch canvas feed: %w", urlErr.Err)
		}
		return nil, nil, fmt.Errorf("fetch canvas feed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("canvas feed returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedSize+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read canvas feed: %w", err)
	}
	if len(body) > maxFeedSize {
		return nil, nil, fmt.Errorf("canvas feed exceeds %d bytes", maxFeedSize)
	}

	events, err := parseICS(bytes.NewReader(body))
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

func newFeedClientConfig(options FeedSecurityOptions) *feedClientConfig {
	return &feedClientConfig{
		client:  newFeedHTTPClient(options),
		options: options,
	}
}

func newFeedHTTPClient(options FeedSecurityOptions) *http.Client {
	networkDialer := &net.Dialer{
		Timeout:   fetchTimeout,
		KeepAlive: 30 * time.Second,
	}
	dialer := &feedDialer{
		allowPrivateNetworks: options.AllowPrivateNetworks,
		lookupNetIP:          net.DefaultResolver.LookupNetIP,
		dialContext:          networkDialer.DialContext,
	}

	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	// A proxy could resolve and connect to the destination itself, bypassing
	// the IP checks in DialContext. Feed requests therefore never use proxy
	// settings inherited from the environment.
	baseTransport.Proxy = nil
	baseTransport.DialContext = dialer.DialContext

	transport := &feedTransport{
		base:    baseTransport,
		options: options,
	}

	return &http.Client{
		Timeout:   fetchTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req == nil {
				return errors.New("canvas feed redirect request is nil")
			}
			// Feed URLs commonly carry access tokens in their query string.
			// Never expose the previous URL through an automatically generated
			// Referer header at the redirect destination.
			req.Header.Del("Referer")
			if len(via) >= 10 {
				return errors.New("canvas feed stopped after 10 redirects")
			}
			return validateFeedURL(req.URL, options)
		},
	}
}

type feedTransport struct {
	base    *http.Transport
	options FeedSecurityOptions
}

func (t *feedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("canvas feed request is nil")
	}
	if err := validateFeedURL(req.URL, t.options); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

func (t *feedTransport) CloseIdleConnections() {
	t.base.CloseIdleConnections()
}

type feedDialer struct {
	allowPrivateNetworks bool
	lookupNetIP          func(context.Context, string, string) ([]netip.Addr, error)
	dialContext          func(context.Context, string, string) (net.Conn, error)
}

func (d *feedDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if ctx == nil {
		return nil, errors.New("canvas feed dial context is nil")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return nil, errors.New("invalid canvas feed destination")
	}

	var addresses []netip.Addr
	if literal, parseErr := netip.ParseAddr(host); parseErr == nil {
		addresses = []netip.Addr{literal}
	} else {
		if d.lookupNetIP == nil {
			return nil, errors.New("canvas feed DNS resolver is not configured")
		}
		addresses, err = d.lookupNetIP(ctx, "ip", host)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("could not resolve canvas feed host")
		}
	}
	if len(addresses) == 0 {
		return nil, errors.New("canvas feed host did not resolve")
	}

	// Validate the complete DNS answer before dialing any address. Rejecting
	// mixed public/private answers prevents fallback from being used to reach
	// an internal destination.
	for _, candidate := range addresses {
		if !candidate.IsValid() || (!d.allowPrivateNetworks && isBlockedFeedAddress(candidate)) {
			return nil, errors.New("canvas feed destination is not publicly routable")
		}
	}

	if d.dialContext == nil {
		return nil, errors.New("canvas feed network dialer is not configured")
	}

	var lastErr error
	for _, candidate := range addresses {
		if network == "tcp4" && !candidate.Unmap().Is4() {
			continue
		}
		if network == "tcp6" && candidate.Unmap().Is4() {
			continue
		}

		destination := net.JoinHostPort(candidate.String(), port)
		conn, dialErr := d.dialContext(ctx, network, destination)
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("connect to canvas feed: %w", lastErr)
	}
	return nil, errors.New("canvas feed host has no usable addresses")
}

func validateFeedURL(feedURL *url.URL, options FeedSecurityOptions) error {
	if feedURL == nil || feedURL.Host == "" || feedURL.Hostname() == "" {
		return errors.New("invalid canvas feed URL")
	}

	switch strings.ToLower(feedURL.Scheme) {
	case "https":
	case "http":
		if !options.AllowHTTP {
			return errors.New("canvas feed URL must use HTTPS")
		}
	default:
		return errors.New("invalid canvas feed URL scheme")
	}

	if literal, err := netip.ParseAddr(feedURL.Hostname()); err == nil &&
		!options.AllowPrivateNetworks && isBlockedFeedAddress(literal) {
		return errors.New("canvas feed destination is not publicly routable")
	}
	return nil
}

var blockedFeedPrefixes = []netip.Prefix{
	// IPv4 special-purpose, private, loopback, link-local, documentation,
	// benchmarking, multicast, and reserved address space.
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	// Azure reserves this virtual address for host-platform communication
	// even though it falls outside the standard private ranges.
	netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),

	// IPv6 unspecified, IPv4-transition, discard, benchmarking,
	// documentation, overlay, private, link-local, and multicast space.
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("100:0:0:1::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func isBlockedFeedAddress(address netip.Addr) bool {
	if !address.IsValid() {
		return true
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() ||
		address.IsPrivate() ||
		address.IsLoopback() ||
		address.IsLinkLocalUnicast() ||
		address.IsLinkLocalMulticast() ||
		address.IsMulticast() ||
		address.IsUnspecified() {
		return true
	}
	for _, prefix := range blockedFeedPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

// parseICS parses an iCal feed from a reader.
func parseICS(r io.Reader) ([]Event, error) {
	lines, err := unfoldLines(r)
	if err != nil {
		return nil, err
	}

	var events []Event
	sawCalendar := false
	endedCalendar := false
	inEvent := false
	propertyCount := 0
	current := map[string]string{}

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "BEGIN:VCALENDAR":
			if sawCalendar {
				return nil, errors.New("multiple or nested iCalendars")
			}
			sawCalendar = true
			endedCalendar = false
			continue
		case line == "END:VCALENDAR":
			if !sawCalendar || endedCalendar || inEvent {
				return nil, errors.New("invalid iCalendar framing")
			}
			endedCalendar = true
			continue
		case line == "BEGIN:VEVENT":
			if !sawCalendar || endedCalendar || inEvent {
				return nil, errors.New("invalid iCalendar event framing")
			}
			inEvent = true
			propertyCount = 0
			current = map[string]string{}
			continue
		case line == "END:VEVENT":
			if !inEvent {
				return nil, errors.New("invalid iCalendar event framing")
			}
			ev := toEvent(current)
			if err := validateEventIdentity(ev); err != nil {
				return nil, err
			}
			if err := validateEventSemantics(ev); err != nil {
				// Reject the complete feed rather than returning the events
				// parsed so far. A partial result must never be mistaken for
				// evidence that later Canvas events were removed.
				return nil, err
			}
			if len(events) >= maxCalendarEvents {
				return nil, fmt.Errorf("iCalendar exceeds %d events", maxCalendarEvents)
			}
			// Keep every VEVENT so the sync worker can distinguish a genuinely
			// removed assignment from an event that became temporarily
			// unclassifiable after a title/category change.
			events = append(events, ev)
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
			return nil, errors.New("invalid iCalendar event property")
		}
		rawPropName := strings.TrimSpace(line[:idx])
		if len(rawPropName) > maxPropertyHeaderSize {
			return nil, fmt.Errorf("iCalendar property header exceeds %d bytes", maxPropertyHeaderSize)
		}
		if len(line)-idx-1 > maxPropertyValueSize {
			return nil, fmt.Errorf("iCalendar property value exceeds %d bytes", maxPropertyValueSize)
		}
		propName := strings.ToUpper(rawPropName)
		propValue := strings.TrimSpace(line[idx+1:])

		// Extract the base property name (before any semicolons)
		baseName := propName
		rawParams := ""
		if semiIdx := strings.Index(rawPropName, ";"); semiIdx > 0 {
			baseName = propName[:semiIdx]
			rawParams = rawPropName[semiIdx+1:]
		}
		if !validPropertyName(baseName) {
			return nil, errors.New("invalid iCalendar property name")
		}
		propertyCount++
		if propertyCount > maxEventProperties {
			return nil, fmt.Errorf("iCalendar event exceeds %d properties", maxEventProperties)
		}

		// Store under both the full property name and the base name
		current[propName] = propValue
		if baseName != propName {
			// Only set base name if not already set (first occurrence wins)
			if _, exists := current[baseName]; !exists {
				current[baseName] = propValue
				current[baseName+"\x00PARAMS"] = rawParams
			}
		}
	}

	if !sawCalendar || !endedCalendar {
		return nil, errors.New("response is not a complete iCalendar")
	}
	if inEvent {
		return nil, errors.New("unterminated iCalendar event")
	}

	return events, nil
}

func validateEventIdentity(event Event) error {
	if event.UID == "" {
		return errors.New("iCalendar event is missing UID")
	}
	if !utf8.ValidString(event.UID) {
		return errors.New("iCalendar event UID is not valid UTF-8")
	}
	if len(event.UID) > maxUIDSize {
		return fmt.Errorf("iCalendar event UID exceeds %d bytes", maxUIDSize)
	}
	for _, character := range event.UID {
		if character < 0x20 || character == 0x7f {
			return errors.New("iCalendar event UID contains a control character")
		}
	}
	return nil
}

func validateEventSemantics(event Event) error {
	// Course identifiers become database keys and Telegram button labels as
	// soon as they are detected, even when an event itself has no due date.
	if event.Course != "" &&
		(len(event.Course) > maxDetectedCourseSize || len(event.CourseName) > maxDetectedCourseSize) {
		return fmt.Errorf("iCalendar detected course text exceeds %d bytes", maxDetectedCourseSize)
	}
	for _, value := range []string{event.Course, event.CourseName} {
		if !utf8.ValidString(value) {
			return errors.New("iCalendar detected course text is not valid UTF-8")
		}
		for _, character := range value {
			if character < 0x20 || character == 0x7f {
				return errors.New("iCalendar detected course text contains a control character")
			}
		}
	}

	// Only structurally actionable events can reach Telegram or Google with
	// their title. In particular, accept the common cancellation tombstone
	// containing only UID + STATUS:CANCELLED so it can safely remove a
	// previously tracked event.
	if !event.Cancelled && event.Course != "" && event.DueAt != nil {
		if !utf8.ValidString(event.Title) {
			return errors.New("iCalendar actionable event title is not valid UTF-8")
		}
		if strings.TrimSpace(event.Title) == "" {
			return errors.New("iCalendar actionable event is missing a title")
		}
		if strings.ContainsRune(event.Title, '\x00') {
			return errors.New("iCalendar actionable event title contains a null byte")
		}
		if len(event.Title) > maxActionableTitleSize {
			return fmt.Errorf("iCalendar actionable event title exceeds %d bytes", maxActionableTitleSize)
		}
	}
	return nil
}

func validPropertyName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		switch {
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '.':
		default:
			return false
		}
	}
	return true
}

// toEvent converts a raw iCal property map to an Event struct.
func toEvent(raw map[string]string) Event {
	title := strings.TrimSpace(unescapeText(raw["SUMMARY"]))
	uid := strings.TrimSpace(raw["UID"])
	categories := strings.TrimSpace(unescapeText(raw["CATEGORIES"]))
	description := strings.TrimSpace(unescapeText(raw["DESCRIPTION"]))
	location := strings.TrimSpace(unescapeText(raw["LOCATION"]))
	icalURL := strings.TrimSpace(raw["URL"])

	// Parse DTSTAMP for change detection
	var dtStamp *time.Time
	if ds := strings.TrimSpace(raw["DTSTAMP"]); ds != "" {
		dtStamp = parseDateWithParams(ds, raw["DTSTAMP\x00PARAMS"])
	}

	// Parse SEQUENCE for change detection
	sequence := 0
	if seqStr := strings.TrimSpace(raw["SEQUENCE"]); seqStr != "" {
		fmt.Sscanf(seqStr, "%d", &sequence)
	}

	due := parseDateWithParams(raw["DTSTART"], raw["DTSTART\x00PARAMS"])
	if due == nil {
		due = parseDateWithParams(raw["DTEND"], raw["DTEND\x00PARAMS"])
	}

	// Detect all-day events (VALUE=DATE format without time)
	isAllDay := hasPropertyParam(raw["DTSTART\x00PARAMS"], "VALUE", "DATE") ||
		(len(strings.TrimSpace(raw["DTSTART"])) == len("20060102") && due != nil)

	courseID, courseName := detectCourse(title, categories, description, icalURL)
	assignmentType := detectType(title)
	status := strings.ToUpper(strings.TrimSpace(raw["STATUS"]))

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
		Status:      status,
		Cancelled:   status == "CANCELLED",
	}
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
		if code := matchCourseCode(courseCodePattern, desc); code != "" {
			courseID = strings.ToUpper(code)
		}
	}

	// Priority 3: SUMMARY bracket code — calendar feed puts [CP2106] at end of title
	titleUpper := strings.TrimSpace(title)
	if courseID == "" && titleUpper != "" {
		if code := matchCourseCode(bracketCodePattern, titleUpper); code != "" {
			courseID = strings.ToUpper(code)
		}
	}

	// Priority 4: SUMMARY regex fallback
	if courseID == "" && titleUpper != "" {
		if code := matchCourseCode(courseCodePattern, titleUpper); code != "" {
			courseID = strings.ToUpper(code)
		}
	}

	// Priority 5: URL field
	if courseID == "" && icalURL != "" {
		if code := matchCourseCode(courseCodePattern, icalURL); code != "" {
			courseID = strings.ToUpper(code)
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
	for _, raw := range candidates {
		if parsed := parseDateWithParams(raw, ""); parsed != nil {
			return parsed
		}
	}
	return nil
}

func parseDateWithParams(raw, rawParams string) *time.Time {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}

	if strings.HasSuffix(value, "Z") {
		if parsed, err := time.Parse("20060102T150405Z", value); err == nil {
			return &parsed
		}
	}
	if parsed, err := time.Parse("20060102T150405-0700", value); err == nil {
		return &parsed
	}

	location := time.UTC
	if timezone := propertyParam(rawParams, "TZID"); timezone != "" {
		if loaded, err := time.LoadLocation(timezone); err == nil {
			location = loaded
		}
	}

	for _, layout := range []string{"20060102T150405", "20060102"} {
		if parsed, err := time.ParseInLocation(layout, value, location); err == nil {
			return &parsed
		}
	}
	return nil
}

func propertyParam(rawParams, name string) string {
	for _, param := range strings.Split(rawParams, ";") {
		key, value, ok := strings.Cut(param, "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return ""
}

func hasPropertyParam(rawParams, name, value string) bool {
	return strings.EqualFold(propertyParam(rawParams, name), value)
}

func unescapeText(value string) string {
	var result strings.Builder
	result.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 >= len(value) {
			result.WriteByte(value[i])
			continue
		}
		i++
		switch value[i] {
		case 'n', 'N':
			result.WriteByte('\n')
		case '\\', ',', ';':
			result.WriteByte(value[i])
		default:
			result.WriteByte('\\')
			result.WriteByte(value[i])
		}
	}
	return result.String()
}

func matchCourseCode(pattern *regexp.Regexp, value string) string {
	match := pattern.FindStringSubmatch(value)
	if len(match) == 0 {
		return ""
	}
	for _, candidate := range match[1:] {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return strings.Trim(strings.TrimSpace(match[0]), "[]")
}

// Init allows configuration of the canvas parser, such as a custom course code regex.
// Call this once at startup before using FetchAndDetect.
func Init(customCourseRegex string) {
	if customCourseRegex != "" {
		compiled, err := regexp.Compile(customCourseRegex)
		if err != nil {
			log.Printf("canvas: invalid CANVASLINK_COURSE_REGEX=%q, using default: %v", customCourseRegex, err)
			return
		}
		courseCodePattern = compiled
		// Also update the bracket code pattern to use the same inner pattern
		if bracketPattern, err := regexp.Compile(`(?i)\[(` + compiled.String() + `)\]\s*$`); err == nil {
			bracketCodePattern = bracketPattern
		}
	}
}

// unfoldLines handles iCal line continuation (RFC 5545).
// Lines starting with a space or tab are continuations of the previous line.
func unfoldLines(r io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(r)
	// Increase buffer for long lines (e.g., X-ALT-DESC with embedded HTML)
	scanner.Buffer(make([]byte, 0, 1024*64), maxPhysicalLineSize)
	var lines []string
	totalSize := 0
	for scanner.Scan() {
		line := scanner.Text()
		totalSize += len(line) + 1
		if totalSize > maxFeedSize {
			return nil, fmt.Errorf("iCalendar exceeds %d bytes", maxFeedSize)
		}
		if len(lines) == 0 {
			lines = append(lines, line)
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if len(lines[len(lines)-1])+len(line)-1 > maxPropertyHeaderSize+maxPropertyValueSize {
				return nil, errors.New("unfolded iCalendar line exceeds size limit")
			}
			lines[len(lines)-1] += line[1:]
			continue
		}
		if len(lines) >= maxCalendarLines {
			return nil, fmt.Errorf("iCalendar exceeds %d unfolded lines", maxCalendarLines)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan iCalendar: %w", err)
	}
	return lines, nil
}
