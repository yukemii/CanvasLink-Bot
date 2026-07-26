package canvas

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

const testCalendar = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:test-1\r\n" +
	"SUMMARY:Assignment [CS2040S]\r\n" +
	"DTSTART:20260726T120000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func developmentFeedClient() (*http.Client, FeedSecurityOptions) {
	options := FeedSecurityOptions{
		AllowHTTP:            true,
		AllowPrivateNetworks: true,
	}
	return newFeedHTTPClient(options), options
}

func TestParseICSRejectsNonCalendar(t *testing.T) {
	t.Parallel()

	if _, err := parseICS(strings.NewReader("<html>not a calendar</html>")); err == nil {
		t.Fatal("parseICS() accepted a non-calendar response")
	}

	const multiple = "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"
	if _, err := parseICS(strings.NewReader(multiple)); err == nil {
		t.Fatal("parseICS() accepted multiple calendar documents")
	}
}

func TestParseICSParsesTZIDAllDayAndEscapedText(t *testing.T) {
	t.Parallel()

	const input = "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:timed-1\r\n" +
		"SUMMARY:Quiz\\, Part 1 [CS2040S]\r\n" +
		"DESCRIPTION:Line one\\nLine two\r\n" +
		"DTSTART;TZID=Asia/Singapore:20260725T153000\r\n" +
		"STATUS:CANCELLED\r\n" +
		"END:VEVENT\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:all-day-1\r\n" +
		"SUMMARY:Assignment [CS2040S]\r\n" +
		"DTSTART;VALUE=DATE:20260726\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	events, err := parseICS(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseICS() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("parseICS() returned %d events, want 2", len(events))
	}

	timed := events[0]
	if timed.Title != "Quiz, Part 1 [CS2040S]" {
		t.Errorf("timed.Title = %q", timed.Title)
	}
	if timed.Description != "Line one\nLine two" {
		t.Errorf("timed.Description = %q", timed.Description)
	}
	if timed.DueAt == nil {
		t.Fatal("timed.DueAt is nil")
	}
	if got := timed.DueAt.Format(time.RFC3339); got != "2026-07-25T15:30:00+08:00" {
		t.Errorf("timed.DueAt = %s", got)
	}
	if timed.Status != "CANCELLED" || !timed.Cancelled {
		t.Errorf("timed cancellation = status %q cancelled %t", timed.Status, timed.Cancelled)
	}

	allDay := events[1]
	if !allDay.AllDay {
		t.Error("allDay.AllDay = false, want true")
	}
	if allDay.DueAt == nil || allDay.DueAt.Format("2006-01-02") != "2026-07-26" {
		t.Errorf("allDay.DueAt = %v", allDay.DueAt)
	}
}

func TestParseICSKeepsUnclassifiedEventsForSafeRemovalDiffs(t *testing.T) {
	t.Parallel()

	const input = "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:unclassified-1\r\n" +
		"SUMMARY:General calendar event\r\n" +
		"DTSTART:20260725T120000Z\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	events, err := parseICS(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseICS() error = %v", err)
	}
	if len(events) != 1 || events[0].UID != "unclassified-1" || events[0].Course != "" {
		t.Fatalf("parseICS() events = %#v", events)
	}
}

func TestParseICSRejectsEventsWithoutSafeIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		uidLine    string
		wantErrSub string
	}{
		{
			name:       "missing UID",
			wantErrSub: "missing UID",
		},
		{
			name:       "oversized UID",
			uidLine:    "UID:" + strings.Repeat("u", maxUIDSize+1) + "\r\n",
			wantErrSub: "UID exceeds",
		},
		{
			name:       "control character in UID",
			uidLine:    "UID:unsafe\x00uid\r\n",
			wantErrSub: "control character",
		},
		{
			name:       "invalid UTF-8 in UID",
			uidLine:    "UID:unsafe\xffuid\r\n",
			wantErrSub: "UID is not valid UTF-8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			input := "BEGIN:VCALENDAR\r\n" +
				"VERSION:2.0\r\n" +
				"BEGIN:VEVENT\r\n" +
				tt.uidLine +
				"SUMMARY:Assignment [CS2040S]\r\n" +
				"DTSTART:20260726T120000Z\r\n" +
				"END:VEVENT\r\n" +
				"END:VCALENDAR\r\n"

			_, err := parseICS(strings.NewReader(input))
			if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("parseICS() error = %v, want substring %q", err, tt.wantErrSub)
			}
		})
	}
}

func TestParseICSRejectsOversizedActionableSemanticFieldsWithoutPartialResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		properties string
		wantErrSub string
	}{
		{
			name: "actionable title",
			properties: "UID:oversized-title\r\n" +
				"CATEGORIES:CS2040S\r\n" +
				"SUMMARY:" + strings.Repeat("t", maxActionableTitleSize+1) + "\r\n" +
				"DTSTART:20260726T120000Z\r\n",
			wantErrSub: "actionable event title exceeds 1024 bytes",
		},
		{
			name: "detected course text",
			properties: "UID:oversized-course\r\n" +
				"CATEGORIES:" + strings.Repeat("c", maxDetectedCourseSize+1) + "\r\n" +
				"SUMMARY:Assignment\r\n" +
				"DTSTART:20260726T120000Z\r\n",
			wantErrSub: "detected course text exceeds 256 bytes",
		},
		{
			name: "missing actionable title",
			properties: "UID:missing-title\r\n" +
				"CATEGORIES:CS2040S\r\n" +
				"SUMMARY:\r\n" +
				"DTSTART:20260726T120000Z\r\n",
			wantErrSub: "actionable event is missing a title",
		},
		{
			name: "control character in course",
			properties: "UID:control-course\r\n" +
				"CATEGORIES:CS2040S\\nINJECTED\r\n" +
				"SUMMARY:Assignment\r\n" +
				"DTSTART:20260726T120000Z\r\n",
			wantErrSub: "course text contains a control character",
		},
		{
			name: "null byte in title",
			properties: "UID:null-title\r\n" +
				"CATEGORIES:CS2040S\r\n" +
				"SUMMARY:Assignment\x00hidden\r\n" +
				"DTSTART:20260726T120000Z\r\n",
			wantErrSub: "title contains a null byte",
		},
		{
			name: "invalid UTF-8 in course",
			properties: "UID:invalid-course-utf8\r\n" +
				"CATEGORIES:CS2040S\xff\r\n" +
				"SUMMARY:Assignment\r\n" +
				"DTSTART:20260726T120000Z\r\n",
			wantErrSub: "course text is not valid UTF-8",
		},
		{
			name: "invalid UTF-8 in actionable title",
			properties: "UID:invalid-title-utf8\r\n" +
				"CATEGORIES:CS2040S\r\n" +
				"SUMMARY:Assignment\xff\r\n" +
				"DTSTART:20260726T120000Z\r\n",
			wantErrSub: "title is not valid UTF-8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			input := "BEGIN:VCALENDAR\r\n" +
				"VERSION:2.0\r\n" +
				"BEGIN:VEVENT\r\n" +
				"UID:safe-event-before-invalid-one\r\n" +
				"CATEGORIES:CS2040S\r\n" +
				"SUMMARY:Assignment\r\n" +
				"DTSTART:20260726T110000Z\r\n" +
				"END:VEVENT\r\n" +
				"BEGIN:VEVENT\r\n" +
				tt.properties +
				"END:VEVENT\r\n" +
				"END:VCALENDAR\r\n"

			events, err := parseICS(strings.NewReader(input))
			if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("parseICS() error = %v, want substring %q", err, tt.wantErrSub)
			}
			if events != nil {
				t.Fatalf("parseICS() returned partial events after semantic failure: %#v", events)
			}
		})
	}
}

func TestParseICSAcceptsSemanticBoundariesAndCancellationTombstone(t *testing.T) {
	t.Parallel()

	input := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:max-semantic-fields\r\n" +
		"CATEGORIES:" + strings.Repeat("c", maxDetectedCourseSize) + "\r\n" +
		"SUMMARY:" + strings.Repeat("t", maxActionableTitleSize) + "\r\n" +
		"DTSTART:20260726T120000Z\r\n" +
		"END:VEVENT\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:cancelled-tombstone\r\n" +
		"STATUS:CANCELLED\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	events, err := parseICS(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseICS() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("parseICS() returned %d events, want 2", len(events))
	}
	if len(events[0].Title) != maxActionableTitleSize ||
		len(events[0].Course) != maxDetectedCourseSize {
		t.Fatalf(
			"boundary event lengths = title:%d course:%d",
			len(events[0].Title),
			len(events[0].Course),
		)
	}
	tombstone := events[1]
	if tombstone.UID != "cancelled-tombstone" || !tombstone.Cancelled ||
		tombstone.Title != "" || tombstone.Course != "" || tombstone.DueAt != nil {
		t.Fatalf("cancellation tombstone = %#v", tombstone)
	}
}

func TestParseICSRejectsMalformedAndExcessiveProperties(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		properties string
		wantErrSub string
	}{
		{
			name:       "missing property separator",
			properties: "MALFORMED-PROPERTY\r\n",
			wantErrSub: "invalid iCalendar event property",
		},
		{
			name:       "invalid property name",
			properties: "INVALID_NAME:value\r\n",
			wantErrSub: "invalid iCalendar property name",
		},
		{
			name:       "oversized property value",
			properties: "DESCRIPTION:" + strings.Repeat("x", maxPropertyValueSize+1) + "\r\n",
			wantErrSub: "property value exceeds",
		},
		{
			name:       "too many properties",
			properties: strings.Repeat("X-SAFE:value\r\n", maxEventProperties+1),
			wantErrSub: "exceeds 512 properties",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			input := "BEGIN:VCALENDAR\r\n" +
				"VERSION:2.0\r\n" +
				"BEGIN:VEVENT\r\n" +
				"UID:safe-uid\r\n" +
				tt.properties +
				"END:VEVENT\r\n" +
				"END:VCALENDAR\r\n"

			_, err := parseICS(strings.NewReader(input))
			if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("parseICS() error = %v, want substring %q", err, tt.wantErrSub)
			}
		})
	}
}

func TestParseICSRejectsExcessiveEventCount(t *testing.T) {
	t.Parallel()

	var input strings.Builder
	input.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n")
	for eventIndex := 0; eventIndex <= maxCalendarEvents; eventIndex++ {
		fmt.Fprintf(
			&input,
			"BEGIN:VEVENT\r\nUID:event-%d\r\nSUMMARY:Assignment [CS2040S]\r\nEND:VEVENT\r\n",
			eventIndex,
		)
	}
	input.WriteString("END:VCALENDAR\r\n")

	_, err := parseICS(strings.NewReader(input.String()))
	if err == nil || !strings.Contains(err.Error(), "exceeds 10000 events") {
		t.Fatalf("parseICS() error = %v, want event-count limit", err)
	}
}

func TestFeedSecurityDefaultsRejectHTTPAndLoopback(t *testing.T) {
	t.Parallel()

	client := newFeedHTTPClient(FeedSecurityOptions{})
	tests := []struct {
		name       string
		feedURL    string
		wantErrSub string
	}{
		{
			name:       "unencrypted HTTP",
			feedURL:    "http://example.com/calendar.ics?access_token=secret",
			wantErrSub: "must use HTTPS",
		},
		{
			name:       "IPv4 loopback",
			feedURL:    "https://127.0.0.1/calendar.ics?access_token=secret",
			wantErrSub: "not publicly routable",
		},
		{
			name:       "IPv6 loopback",
			feedURL:    "https://[::1]/calendar.ics?access_token=secret",
			wantErrSub: "not publicly routable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := fetchAndDetect(context.Background(), client, tt.feedURL)
			if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("fetchAndDetect() error = %v, want substring %q", err, tt.wantErrSub)
			}
			if strings.Contains(err.Error(), "access_token=secret") {
				t.Fatalf("fetchAndDetect() leaked feed credentials: %v", err)
			}
		})
	}
}

func TestFeedSecurityExplicitDevelopmentOverrideSupportsHTTPTestServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/calendar")
		_, _ = w.Write([]byte(testCalendar))
	}))
	defer server.Close()

	ConfigureFeedSecurity(FeedSecurityOptions{
		AllowHTTP:            true,
		AllowPrivateNetworks: true,
	})
	defer ConfigureFeedSecurity(FeedSecurityOptions{})

	events, seeds, err := FetchAndDetectContext(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("FetchAndDetectContext() error = %v", err)
	}
	if len(events) != 1 || events[0].UID != "test-1" {
		t.Fatalf("events = %#v, want one parsed event", events)
	}
	if len(seeds) != 1 || seeds[0].CourseID != "CS2040S" {
		t.Fatalf("seeds = %#v, want one CS2040S seed", seeds)
	}
}

func TestFeedSecurityOverridesAreIndependent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options FeedSecurityOptions
		feedURL string
		wantErr bool
	}{
		{
			name:    "HTTP override permits public HTTP",
			options: FeedSecurityOptions{AllowHTTP: true},
			feedURL: "http://1.1.1.1/feed.ics",
		},
		{
			name:    "HTTP override does not permit private destinations",
			options: FeedSecurityOptions{AllowHTTP: true},
			feedURL: "http://127.0.0.1/feed.ics",
			wantErr: true,
		},
		{
			name:    "private override permits HTTPS loopback",
			options: FeedSecurityOptions{AllowPrivateNetworks: true},
			feedURL: "https://127.0.0.1/feed.ics",
		},
		{
			name:    "private override does not permit HTTP",
			options: FeedSecurityOptions{AllowPrivateNetworks: true},
			feedURL: "http://127.0.0.1/feed.ics",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			request, err := http.NewRequest(http.MethodGet, tt.feedURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = validateFeedURL(request.URL, tt.options)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateFeedURL() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestFeedHTTPClientValidatesEveryRedirect(t *testing.T) {
	t.Parallel()

	client := newFeedHTTPClient(FeedSecurityOptions{})
	origin, err := http.NewRequest(http.MethodGet, "https://example.com/feed.ics", nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		target     string
		wantErrSub string
	}{
		{
			name:       "HTTPS downgrade",
			target:     "http://example.net/feed.ics",
			wantErrSub: "must use HTTPS",
		},
		{
			name:       "private IPv4",
			target:     "https://10.0.0.1/feed.ics",
			wantErrSub: "not publicly routable",
		},
		{
			name:       "link-local IPv6",
			target:     "https://[fe80::1]/feed.ics",
			wantErrSub: "not publicly routable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			redirect, requestErr := http.NewRequest(http.MethodGet, tt.target, nil)
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			err := client.CheckRedirect(redirect, []*http.Request{origin})
			if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("CheckRedirect() error = %v, want substring %q", err, tt.wantErrSub)
			}
		})
	}

	tooMany, err := http.NewRequest(http.MethodGet, "https://example.net/feed.ics", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(tooMany, make([]*http.Request, 10)); err == nil {
		t.Fatal("CheckRedirect() accepted more than 10 redirects")
	}

	safeRedirect, err := http.NewRequest(http.MethodGet, "https://example.net/feed.ics", nil)
	if err != nil {
		t.Fatal(err)
	}
	safeRedirect.Header.Set("Referer", "https://example.com/feed.ics?access_token=secret")
	if err := client.CheckRedirect(safeRedirect, []*http.Request{origin}); err != nil {
		t.Fatalf("CheckRedirect() rejected safe redirect: %v", err)
	}
	if referer := safeRedirect.Header.Get("Referer"); referer != "" {
		t.Fatalf("CheckRedirect() retained sensitive Referer %q", referer)
	}
}

func TestBlockedFeedAddressRanges(t *testing.T) {
	t.Parallel()

	blocked := []string{
		"0.0.0.1",
		"10.0.0.1",
		"100.64.0.1",
		"127.0.0.1",
		"168.63.129.16",
		"169.254.1.1",
		"172.16.0.1",
		"192.0.0.1",
		"192.0.2.1",
		"192.168.0.1",
		"198.18.0.1",
		"198.51.100.1",
		"203.0.113.1",
		"224.0.0.1",
		"240.0.0.1",
		"::",
		"::1",
		"::ffff:127.0.0.1",
		"64:ff9b::7f00:1",
		"100::1",
		"100:0:0:1::1",
		"2001:1::1",
		"2001:2::1",
		"2001:db8::1",
		"2002::1",
		"3fff::1",
		"5f00::1",
		"fc00::1",
		"fe80::1",
		"fec0::1",
		"ff02::1",
	}
	for _, rawAddress := range blocked {
		address := netip.MustParseAddr(rawAddress)
		if !isBlockedFeedAddress(address) {
			t.Errorf("isBlockedFeedAddress(%s) = false, want true", rawAddress)
		}
	}

	public := []string{
		"1.1.1.1",
		"8.8.8.8",
		"2606:4700:4700::1111",
		"2001:4860:4860::8888",
	}
	for _, rawAddress := range public {
		address := netip.MustParseAddr(rawAddress)
		if isBlockedFeedAddress(address) {
			t.Errorf("isBlockedFeedAddress(%s) = true, want false", rawAddress)
		}
	}
}

func TestFeedDialerRejectsMixedDNSAnswerBeforeDialing(t *testing.T) {
	t.Parallel()

	dialCalled := false
	dialer := &feedDialer{
		lookupNetIP: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("93.184.216.34"),
				netip.MustParseAddr("127.0.0.1"),
			}, nil
		},
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			dialCalled = true
			return nil, errors.New("unexpected dial")
		},
	}

	_, err := dialer.DialContext(context.Background(), "tcp", "canvas.example:443")
	if err == nil || !strings.Contains(err.Error(), "not publicly routable") {
		t.Fatalf("DialContext() error = %v, want public routing error", err)
	}
	if dialCalled {
		t.Fatal("DialContext() attempted a connection before validating the complete DNS answer")
	}
}

func TestFeedDialerDialsResolvedIPAddressNotHostname(t *testing.T) {
	t.Parallel()

	var dialedAddress string
	dialFailure := errors.New("test dial stopped")
	dialer := &feedDialer{
		lookupNetIP: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		dialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			dialedAddress = address
			return nil, dialFailure
		},
	}

	_, err := dialer.DialContext(context.Background(), "tcp", "canvas.example:443")
	if !errors.Is(err, dialFailure) {
		t.Fatalf("DialContext() error = %v, want wrapped test failure", err)
	}
	if dialedAddress != "93.184.216.34:443" {
		t.Fatalf("DialContext() dialed %q, want resolved IP address", dialedAddress)
	}
}

func TestFeedHTTPClientDisablesProxies(t *testing.T) {
	t.Parallel()

	client := newFeedHTTPClient(FeedSecurityOptions{})
	transport, ok := client.Transport.(*feedTransport)
	if !ok {
		t.Fatalf("client transport type = %T", client.Transport)
	}
	if transport.base.Proxy != nil {
		t.Fatal("feed HTTP transport has a proxy function; proxy could bypass destination validation")
	}
}

func TestFetchErrorDoesNotLeakFeedURL(t *testing.T) {
	t.Parallel()

	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("network unavailable")
		}),
	}
	const feedURL = "https://example.com/calendar.ics?access_token=top-secret"
	_, _, err := fetchAndDetect(context.Background(), client, feedURL)
	if err == nil {
		t.Fatal("fetchAndDetect() returned nil error")
	}
	if strings.Contains(err.Error(), "top-secret") || strings.Contains(err.Error(), feedURL) {
		t.Fatalf("fetchAndDetect() leaked feed URL credentials: %v", err)
	}
}

func TestFetchAndDetectContextLimitsResponseSize(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\n"))
		_, _ = w.Write([]byte(strings.Repeat("x", maxFeedSize)))
		_, _ = w.Write([]byte("\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()

	client, options := developmentFeedClient()
	_, _, err := fetchAndDetectWithOptions(context.Background(), client, server.URL, options)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("fetchAndDetect() error = %v, want size error", err)
	}
}

func TestFetchAndDetectContextHonorsCancellation(t *testing.T) {
	t.Parallel()

	requestStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	client, options := developmentFeedClient()
	go func() {
		_, _, err := fetchAndDetectWithOptions(ctx, client, server.URL, options)
		done <- err
	}()

	<-requestStarted
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("fetchAndDetect() returned nil after cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fetchAndDetect() did not stop after cancellation")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
