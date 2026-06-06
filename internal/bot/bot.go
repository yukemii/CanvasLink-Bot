package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/markadodo/canvaslink/internal/canvas"
	canvasGoogle "github.com/markadodo/canvaslink/internal/google"
	"github.com/markadodo/canvaslink/internal/oauth"
	"github.com/markadodo/canvaslink/internal/store"
)

// Onboarding status constants
const (
	OnboardingAwaitingURL  = "awaiting_url"
	OnboardingGooglePrompt = "google_prompt"
	OnboardingCourseSetup  = "course_setup"
)

// Callback data prefixes
const (
	CBPrefixOnboardGoogleYes = "ob_google_yes"
	CBPrefixOnboardGoogleNo  = "ob_google_no"
	CBPrefixOnboardMode      = "ob_mode|"
	CBPrefixOnboardDone      = "ob_done"
	CBPrefixSettingsHub      = "settings|"
	CBPrefixIntegrations     = "integrations|"
	CBPrefixPreferences      = "prefs|"
	CBPrefixSyncNow          = "sync_now"
	CBPrefixClose            = "close"
	CBPrefixCourse           = "course|"
	CBPrefixModules          = "modules"
	CBPrefixMode             = "mode|"
	CBPrefixModeSelect       = "mode_select|"
	CBPrefixPending          = "pending|"
	CBPrefixNoop             = "noop|"
	CBPrefixConfirm          = "confirm|"
	CBPrefixCancel           = "cancel"
	CBPrefixInterval         = "interval|"
	CBPrefixNewCourseMode    = "new_course_mode|" // new_course_mode|COURSE_ID|TYPE|TYPE_INDEX|TOTAL_TYPES|MODE

	// Transition dialog prefixes (settings mode changes)
	CBPrefixSyncPast         = "sync_past|"   // sync_past|COURSE_ID|TYPE|TARGET_MODE
	CBPrefixRemoveCal        = "remove_cal|"  // remove_cal|COURSE_ID|TYPE|TARGET_MODE
	CBPrefixHistorical       = "historical|"  // historical|COURSE_ID|TYPE|TARGET_MODE
)

type Bot struct {
	api         *tgbotapi.BotAPI
	store       *store.Store
	oauthServer *oauth.Server
	google      *canvasGoogle.CalendarClient

	// pendingOAuth tracks users waiting for Google OAuth to complete during onboarding.
	// Key: telegram user ID, Value: chat ID.
	// After OAuth callback succeeds, the bot will start course setup for these users.
	pendingOAuth map[int64]int64
	// oauthNotifyCh is used by the OAuth callback handler to signal that a user completed auth.
	oauthNotifyCh chan int64
}

func New(token string, db *store.Store, oauthServer *oauth.Server, googleClient *canvasGoogle.CalendarClient) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}
	return &Bot{
		api:           api,
		store:         db,
		oauthServer:   oauthServer,
		google:        googleClient,
		pendingOAuth:  make(map[int64]int64),
		oauthNotifyCh: make(chan int64, 100),
	}, nil
}

func (b *Bot) Start(ctx context.Context) error {
	log.Printf("CanvasLink bot is online as @%s", b.api.Self.UserName)
	_ = b.registerCommands()

	updates := b.api.GetUpdatesChan(tgbotapi.NewUpdate(0))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case update := <-updates:
			if update.CallbackQuery != nil {
				b.handleCallback(ctx, update.CallbackQuery)
				continue
			}
			if update.Message == nil || update.Message.From == nil {
				continue
			}
			b.handleMessage(ctx, update.Message)
		case userID := <-b.oauthNotifyCh:
			// A user completed Google OAuth — start course setup if they're in onboarding
			b.handleOAuthComplete(ctx, userID)
		}
	}
}

// NotifyOAuthComplete is called by the OAuth callback handler when a user
// successfully connects Google Calendar during onboarding.
func (b *Bot) NotifyOAuthComplete(telegramUserID int64) {
	b.oauthNotifyCh <- telegramUserID
}

func (b *Bot) handleOAuthComplete(ctx context.Context, userID int64) {
	chatID, ok := b.pendingOAuth[userID]
	if !ok {
		return // not in onboarding
	}
	delete(b.pendingOAuth, userID)

	// Clear onboarding status so the user can proceed
	if err := b.store.SetOnboardingStatus(ctx, userID, ""); err != nil {
		log.Printf("clear onboarding status failed: %v", err)
	}

	// Send confirmation + modes guide + first prompt all at once
	b.reply(chatID, "✅ Google Calendar connected! Now let's set up your sync preferences.")
	b.sendModesGuide(ctx, chatID, userID)
}

// ---------------------------------------------------------------------------
// Message handler
// ---------------------------------------------------------------------------

func (b *Bot) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	userID := int64(msg.From.ID)
	text := strings.TrimSpace(msg.Text)

	// Check if this is a command
	if strings.HasPrefix(text, "/") {
		b.handleCommand(ctx, msg, userID, text)
		return
	}

	// Non-command text — check if user is in onboarding
	account, err := b.store.GetTelegramAccount(ctx, userID)
	if err != nil {
		log.Printf("get account failed: %v", err)
		return
	}

	if account != nil && account.OnboardingStatus == OnboardingAwaitingURL {
		// Treat as potential iCal URL
		b.handleOnboardingURL(ctx, msg, userID, text)
		return
	}

	// Default fallback
	b.reply(msg.Chat.ID, "Try /settings or /start to get started.")
}

func (b *Bot) handleCommand(ctx context.Context, msg *tgbotapi.Message, userID int64, text string) {
	switch {
	case text == "/start":
		b.handleStart(ctx, msg, userID)
	case text == "/help":
		b.handleHelp(ctx, msg)
	case text == "/settings":
		b.handleSettings(ctx, msg, userID)
	default:
		b.reply(msg.Chat.ID, "Unknown command. Try /start, /help, or /settings.")
	}
}

// ---------------------------------------------------------------------------
// /start — Onboarding wizard
// ---------------------------------------------------------------------------

func (b *Bot) handleStart(ctx context.Context, msg *tgbotapi.Message, userID int64) {
	// Ensure telegram account exists
	if err := b.store.UpsertTelegramAccount(ctx, userID, msg.Chat.ID, msg.From.UserName); err != nil {
		log.Printf("upsert telegram account failed: %v", err)
	}

	// Check if user already has a feed
	hasFeed, err := b.store.HasFeed(ctx, userID)
	if err != nil {
		log.Printf("has feed check failed: %v", err)
		b.reply(msg.Chat.ID, "Something went wrong. Please try again.")
		return
	}

	if hasFeed {
		// Already onboarded — show main menu
		b.reply(msg.Chat.ID, "Welcome back! Use /settings to configure your courses, or /help for a guide.")
		return
	}

	// Start onboarding
	if err := b.store.SetOnboardingStatus(ctx, userID, OnboardingAwaitingURL); err != nil {
		log.Printf("set onboarding status failed: %v", err)
	}

	b.reply(msg.Chat.ID, `🎓 Welcome to CanvasLink!

I'll sync your Canvas assignments to Google Calendar automatically.

First, let's connect your Canvas account.

📋 How to get your Canvas link:
1. Log into Canvas
2. Open the Calendar page
3. Click the "Calendar Feed" button/link (usually on the right side)
4. Copy the feed URL that appears

Then just paste the link here and I'll take it from there!`)
}

// ---------------------------------------------------------------------------
// /help — Full user guide
// ---------------------------------------------------------------------------

func (b *Bot) handleHelp(ctx context.Context, msg *tgbotapi.Message) {
	b.reply(msg.Chat.ID, `📖 *CanvasLink User Guide*

CanvasLink syncs your Canvas course events to Google Calendar automatically.

*Getting Started*
1. Send /start to begin onboarding
2. Paste your Canvas Calendar Feed URL when prompted
3. Choose whether to connect Google Calendar
4. Set sync preferences for each course and event type

*Sync Modes*
Each course has event types (assignments, quizzes, exams, etc.).
You can set how each type behaves:

🔇 *Quiet Sync* — Events are automatically added to your Google Calendar. No Telegram notification.
🔔 *Notify & Sync* — Events are added to your Google Calendar AND you get a Telegram notification.
📋 *Review* — You'll receive a notification here asking if you want to add it to your calendar.
🚫 *Ignore* — Events are filtered out and ignored.

*Commands*
/start — Begin or restart onboarding
/help — Show this guide
/settings — Open the settings dashboard

*Settings Dashboard*
/settings shows your current connection status and lets you:

🔗 *Integrations* — Manage Canvas and Google connections
  • Reconnect/Disconnect Canvas
  • Reconnect/Disconnect Google
  • Reset / Wipe (clears CanvasLink data + removes CanvasLink events from your Google Calendar)

⚙️ *Preferences* — Customize your experience
  • Sync Frequency — How often I check for new events (15min to 24hr)
  • Course Sync Modes — Change how each course's event types are handled

🔄 *Sync Now* — Trigger an immediate sync and see what changed

*Need help?*
If you run into issues, make sure:
1. Your Canvas Calendar Feed URL is valid (starts with https:// and ends in .ics)
2. Google Calendar is connected if you want Quiet Sync or Notify & Sync
3. The OAuth redirect URI is set correctly in Google Cloud Console`)
}

// ---------------------------------------------------------------------------
// /settings — Status dashboard
// ---------------------------------------------------------------------------

func (b *Bot) handleSettings(ctx context.Context, msg *tgbotapi.Message, userID int64) {
	hasFeed, err := b.store.HasFeed(ctx, userID)
	if err != nil {
		log.Printf("has feed check failed: %v", err)
		b.reply(msg.Chat.ID, "Something went wrong.")
		return
	}
	if !hasFeed {
		b.reply(msg.Chat.ID, "No Canvas feed connected yet. Send /start to begin setup.")
		return
	}

	statusText := b.buildStatusText(ctx, userID)
	replyMsg := tgbotapi.NewMessage(msg.Chat.ID, statusText)
	replyMsg.ParseMode = "Markdown"
	replyMsg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔗 Integrations", CBPrefixSettingsHub+"integrations"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Preferences", CBPrefixSettingsHub+"preferences"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Sync Now", CBPrefixSyncNow),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Close", CBPrefixClose),
		),
	)
	if _, err := b.api.Send(replyMsg); err != nil {
		log.Printf("telegram send failed: %v", err)
	}
}

func (b *Bot) buildStatusText(ctx context.Context, userID int64) string {
	var sb strings.Builder
	sb.WriteString("📊 *CanvasLink Dashboard*\n\n")

	// Canvas status
	hasFeed, _ := b.store.HasFeed(ctx, userID)
	if hasFeed {
		sb.WriteString("📡 Canvas: ✅ Connected\n")
	} else {
		sb.WriteString("📡 Canvas: ❌ Not connected\n")
	}

	// Google status
	googleConnected, _ := b.oauthServer.IsConnected(ctx, userID)
	if googleConnected {
		sb.WriteString("🔗 Google: ✅ Connected\n")
	} else {
		sb.WriteString("🔗 Google: ❌ Not connected\n")
	}

	// Last sync time
	feed, _ := b.store.GetFeed(ctx, userID)
	if feed != nil && feed.LastSyncedAt != nil {
		ago := time.Since(*feed.LastSyncedAt).Round(time.Second)
		if ago < time.Minute {
			sb.WriteString(fmt.Sprintf("🕐 Last Sync: Just now\n"))
		} else if ago < time.Hour {
			sb.WriteString(fmt.Sprintf("🕐 Last Sync: %d min ago\n", int(ago.Minutes())))
		} else {
			sb.WriteString(fmt.Sprintf("🕐 Last Sync: %d hr %d min ago\n", int(ago.Hours()), int(ago.Minutes())%60))
		}
	} else {
		sb.WriteString("🕐 Last Sync: Never\n")
	}

	// Sync frequency
	interval, _ := b.store.GetSyncInterval(ctx, userID)
	sb.WriteString(fmt.Sprintf("⏱ Sync Frequency: Every %s\n\n", interval))

	// Courses and their modes
	courses, err := b.store.ListUserCourses(ctx, userID)
	if err == nil && len(courses) > 0 {
		sb.WriteString("📚 *Courses:*\n")
		for _, c := range courses {
			label := c.CourseID
			if c.CourseName != "" && c.CourseName != c.CourseID {
				label = c.CourseName
			}
			settings, err := b.store.ListCourseSettings(ctx, userID, c.CourseID)
			if err == nil {
				var modeStrs []string
				for _, s := range settings {
					emoji := modeEmoji(s.Mode)
					modeStrs = append(modeStrs, fmt.Sprintf("%s %s", emoji, typeLabel(s.AssignmentType)))
				}
				sb.WriteString(fmt.Sprintf("  • %s — %s\n", label, strings.Join(modeStrs, ", ")))
			}
		}
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// Settings callbacks
// ---------------------------------------------------------------------------

func (b *Bot) handleSettingsCallback(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, action string) {
	b.answerCallback(cq.ID, "")

	switch action {
	case "integrations":
		b.showIntegrationsMenu(ctx, cq, userID)
	case "preferences":
		b.showPreferencesMenu(ctx, cq, userID)
	default:
		b.reply(cq.Message.Chat.ID, "Unknown option.")
	}
}

// ---------------------------------------------------------------------------
// Integrations menu
// ---------------------------------------------------------------------------

func (b *Bot) showIntegrationsMenu(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64) {
	hasFeed, _ := b.store.HasFeed(ctx, userID)
	googleConnected, _ := b.oauthServer.IsConnected(ctx, userID)

	var sb strings.Builder
	sb.WriteString("🔗 *Integrations*\n\n")
	if hasFeed {
		sb.WriteString("📡 Canvas: ✅ Connected\n")
	} else {
		sb.WriteString("📡 Canvas: ❌ Not connected\n")
	}
	if googleConnected {
		sb.WriteString("🔗 Google: ✅ Connected\n")
	} else {
		sb.WriteString("🔗 Google: ❌ Not connected\n")
	}

	var rows [][]tgbotapi.InlineKeyboardButton

	if hasFeed {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📡 Reconnect Canvas", CBPrefixIntegrations+"reconnect_canvas"),
		))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📡 Disconnect Canvas", CBPrefixIntegrations+"disconnect_canvas"),
		))
	} else {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📡 Connect Canvas", CBPrefixIntegrations+"connect_canvas"),
		))
	}

	if googleConnected {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔗 Reconnect Google", CBPrefixIntegrations+"reconnect_google"),
		))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔗 Disconnect Google", CBPrefixIntegrations+"disconnect_google"),
		))
	} else {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔗 Connect Google", CBPrefixIntegrations+"connect_google"),
		))
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("⚠️ Reset / Wipe", CBPrefixIntegrations+"reset"),
	))
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 Back", CBPrefixSettingsHub+"main"),
	))

	edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, sb.String())
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("telegram edit failed: %v", err)
	}
}

func (b *Bot) handleIntegrationsCallback(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, action string) {
	b.answerCallback(cq.ID, "")

	switch action {
	case "connect_canvas":
		// Start onboarding flow for Canvas
		if err := b.store.SetOnboardingStatus(ctx, userID, OnboardingAwaitingURL); err != nil {
			log.Printf("set onboarding status failed: %v", err)
		}
		b.reply(cq.Message.Chat.ID, "Please paste your Canvas Calendar Feed URL to connect.")
		// Edit the integrations menu to show the prompt
		edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, "📡 Please paste your Canvas Calendar Feed URL below.")
		if _, err := b.api.Send(edit); err != nil {
			log.Printf("telegram edit failed: %v", err)
		}

	case "reconnect_canvas":
		hasFeed, _ := b.store.HasFeed(ctx, userID)
		if !hasFeed {
			b.reply(cq.Message.Chat.ID, "Canvas is not currently connected. Use Connect Canvas instead.")
			return
		}
		if err := b.store.SetOnboardingStatus(ctx, userID, OnboardingAwaitingURL); err != nil {
			log.Printf("set onboarding status failed: %v", err)
		}
		b.reply(cq.Message.Chat.ID, "Please paste your new Canvas Calendar Feed URL to reconnect.")
		edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, "📡 Please paste your new Canvas Calendar Feed URL below.")
		if _, err := b.api.Send(edit); err != nil {
			log.Printf("telegram edit failed: %v", err)
		}

	case "disconnect_canvas":
		hasFeed, _ := b.store.HasFeed(ctx, userID)
		if !hasFeed {
			b.reply(cq.Message.Chat.ID, "Canvas is already disconnected.")
			return
		}
		b.sendConfirmDialog(ctx, cq.Message.Chat.ID, "disconnect_canvas",
			`⚠️ Disconnect Canvas?

This will:
• Remove your Canvas feed URL
• Delete all your sync settings (course modes)
• Events already in Google Calendar will NOT be removed

You'll need to go through onboarding again to reconnect.

Are you sure?`)

	case "connect_google":
		googleConnected, _ := b.oauthServer.IsConnected(ctx, userID)
		if googleConnected {
			b.reply(cq.Message.Chat.ID, "Google Calendar is already connected.")
			return
		}
		b.sendGoogleAuthLink(ctx, cq.Message.Chat.ID, userID)

	case "reconnect_google":
		googleConnected, _ := b.oauthServer.IsConnected(ctx, userID)
		if !googleConnected {
			b.reply(cq.Message.Chat.ID, "Google Calendar is not currently connected. Use Connect Google instead.")
			return
		}
		b.sendGoogleAuthLink(ctx, cq.Message.Chat.ID, userID)

	case "disconnect_google":
		googleConnected, _ := b.oauthServer.IsConnected(ctx, userID)
		if !googleConnected {
			b.reply(cq.Message.Chat.ID, "Google Calendar is already disconnected.")
			return
		}
		b.sendConfirmDialog(ctx, cq.Message.Chat.ID, "disconnect_google",
			`⚠️ Disconnect Google Calendar?

This will:
• Remove your Google Calendar connection
• Events already in Google Calendar will NOT be removed
• New events will still be detected but won't sync to Google

Are you sure?`)

	case "reset":
		b.sendConfirmDialog(ctx, cq.Message.Chat.ID, "reset",
			`⚠️⚠️ RESET ALL DATA ⚠️⚠️

This will:
• Disconnect Canvas feed
• Disconnect Google Calendar
• Delete ALL settings, synced events, and pending actions
• Remove CanvasLink events from your Google Calendar
• You'll need to go through the full onboarding again

Are you absolutely sure?`)
	}
}

func (b *Bot) sendGoogleAuthLink(ctx context.Context, chatID int64, userID int64) {
	authURL, err := b.oauthServer.AuthURL(ctx, userID)
	if err != nil {
		log.Printf("google auth url generation failed: %v", err)
		b.reply(chatID, "I could not get a Google connect link right now.")
		return
	}
	b.reply(chatID, fmt.Sprintf("🔗 Click here to connect Google Calendar:\n%s", authURL))
}

// ---------------------------------------------------------------------------
// Preferences menu
// ---------------------------------------------------------------------------

func (b *Bot) showPreferencesMenu(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64) {
	interval, _ := b.store.GetSyncInterval(ctx, userID)

	var sb strings.Builder
	sb.WriteString("⚙️ *Preferences*\n\n")
	sb.WriteString(fmt.Sprintf("⏱ Current Sync Frequency: Every %s\n\n", interval))
	sb.WriteString("Choose an option below:")

	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("⏱ Sync Frequency", CBPrefixPreferences+"frequency"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("📚 Course Sync Modes", CBPrefixPreferences+"courses"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", CBPrefixSettingsHub+"main"),
		},
	}

	edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, sb.String())
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("telegram edit failed: %v", err)
	}
}

func (b *Bot) handlePreferencesCallback(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, action string) {
	b.answerCallback(cq.ID, "")

	switch action {
	case "frequency":
		b.showFrequencyMenu(ctx, cq, userID)
	case "courses":
		b.sendModulesMenu(ctx, userID, cq.Message.Chat.ID)
	}
}

func (b *Bot) showFrequencyMenu(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64) {
	currentInterval, _ := b.store.GetSyncInterval(ctx, userID)

	var sb strings.Builder
	sb.WriteString("⏱ *Sync Frequency*\n\n")
	sb.WriteString(fmt.Sprintf("Current: Every %s\n\n", currentInterval))
	sb.WriteString("How often should I check for new events?")

	options := []string{"15m", "30m", "1h", "2h", "6h", "12h", "24h"}
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, opt := range options {
		label := opt
		if opt == currentInterval {
			label = "✅ " + opt
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, CBPrefixInterval+opt),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 Back", CBPrefixPreferences+"main"),
	))

	edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, sb.String())
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("telegram edit failed: %v", err)
	}
}

func (b *Bot) handleIntervalCallback(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, interval string) {
	if err := b.store.UpdateSyncInterval(ctx, userID, interval); err != nil {
		log.Printf("update sync interval failed: %v", err)
		b.answerCallback(cq.ID, "Failed")
		return
	}
	b.answerCallback(cq.ID, fmt.Sprintf("Sync frequency set to every %s", interval))
	b.showFrequencyMenu(ctx, cq, userID)
}

// ---------------------------------------------------------------------------
// Sync Now
// ---------------------------------------------------------------------------

func (b *Bot) handleSyncNow(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64) {
	b.answerCallback(cq.ID, "Syncing...")

	feed, err := b.store.GetFeed(ctx, userID)
	if err != nil || feed == nil {
		b.reply(cq.Message.Chat.ID, "No Canvas feed found. Connect one with /start first.")
		return
	}

	// Fetch and parse
	events, seeds, err := canvas.FetchAndDetect(feed.ICalURL)
	if err != nil {
		log.Printf("sync now: fetch failed: %v", err)
		b.reply(cq.Message.Chat.ID, "Failed to fetch Canvas feed. The URL might be invalid.")
		return
	}

	// Auto-seed new courses
	if err := b.store.SeedDefaultSettings(ctx, userID, seeds); err != nil {
		log.Printf("sync now: seed settings failed: %v", err)
	}

	// Get previously synced events
	prevSynced, err := b.store.ListSyncedEvents(ctx, userID)
	if err != nil {
		log.Printf("sync now: list synced failed: %v", err)
		b.reply(cq.Message.Chat.ID, "Failed to check previously synced events.")
		return
	}

	now := time.Now()

	// Build current UID set, skipping past events
	currentUIDs := make(map[string]canvas.Event, len(events))
	for _, ev := range events {
		if ev.UID == "" || ev.Course == "" || ev.DueAt == nil {
			continue
		}
		if ev.DueAt.Before(now) {
			continue
		}
		currentUIDs[ev.UID] = ev
	}

	newCount := 0
	updatedCount := 0
	removedCount := 0

	// Process new/changed
	for uid, ev := range currentUIDs {
		prev, exists := prevSynced[uid]
		if !exists {
			// New event
			mode, err := b.store.GetCourseTypeMode(ctx, userID, ev.Course, ev.Type)
			if err != nil || mode == store.ModeIgnore {
				continue
			}
			if mode == store.ModeQuiet || mode == store.ModeNotify {
				calendarID, eventID, err := b.google.CreateEventForTelegramUser(
					ctx, userID, ev.Title, *ev.DueAt,
					fmt.Sprintf("CanvasLink sync\nCourse: %s\nType: %s", ev.Course, ev.Type),
				)
				if err == nil {
					b.store.UpsertSyncedEvent(ctx, store.SyncedEventInput{
						TelegramUserID:   userID,
						CourseID:         ev.Course,
						AssignmentType:   ev.Type,
						CanvasEventUID:   ev.UID,
						CanvasTitle:      ev.Title,
						CanvasDueAt:      *ev.DueAt,
						GoogleCalendarID: calendarID,
						GoogleEventID:    eventID,
						CanvasDTStamp:    ev.DTStamp,
						CanvasSequence:   ev.Sequence,
					})
					newCount++
				}
			} else if mode == store.ModeReview {
				// Create pending action
				account, _ := b.store.GetTelegramAccount(ctx, userID)
				if account != nil {
					b.store.CreatePendingActionIfAbsent(ctx, store.PendingActionInput{
						TelegramUserID: userID,
						CourseID:       ev.Course,
						AssignmentType: ev.Type,
						CanvasEventUID: ev.UID,
						CanvasTitle:    ev.Title,
						CanvasDueAt:    *ev.DueAt,
						TelegramChatID: account.ChatID,
					})
					newCount++
				}
			}
		} else {
			// Check if changed
			changed := false
			if ev.DTStamp != nil && prev.CanvasDTStamp != nil && !ev.DTStamp.Equal(*prev.CanvasDTStamp) {
				changed = true
			}
			if ev.Sequence > prev.CanvasSequence {
				changed = true
			}
			if ev.Title != prev.CanvasTitle {
				changed = true
			}
			if ev.DueAt != nil && !ev.DueAt.Equal(prev.CanvasDueAt) {
				changed = true
			}
			if changed {
				// Update the synced event record
				b.store.UpsertSyncedEvent(ctx, store.SyncedEventInput{
					TelegramUserID:   userID,
					CourseID:         ev.Course,
					AssignmentType:   ev.Type,
					CanvasEventUID:   ev.UID,
					CanvasTitle:      ev.Title,
					CanvasDueAt:      *ev.DueAt,
					GoogleCalendarID: prev.GoogleCalendarID,
					GoogleEventID:    prev.GoogleEventID,
					CanvasDTStamp:    ev.DTStamp,
					CanvasSequence:   ev.Sequence,
				})
				updatedCount++
			}
		}
	}

	// Detect removed events
	for uid := range prevSynced {
		if _, stillExists := currentUIDs[uid]; !stillExists {
			b.store.DeleteSyncedEvent(ctx, userID, uid)
			removedCount++
		}
	}

	// Update last sync time
	b.store.TouchFeedSync(ctx, userID)

	// Send summary
	summary := fmt.Sprintf("✅ *Sync Complete*\n\nNew Events: %d\nUpdated Events: %d\nRemoved Events: %d",
		newCount, updatedCount, removedCount)
	b.reply(cq.Message.Chat.ID, summary)
}

// ---------------------------------------------------------------------------
// Onboarding: URL paste handler
// ---------------------------------------------------------------------------

func (b *Bot) handleOnboardingURL(ctx context.Context, msg *tgbotapi.Message, userID int64, text string) {
	// Validate it looks like a URL
	parsedURL, err := url.ParseRequestURI(text)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		b.reply(msg.Chat.ID, "That doesn't look like a valid URL. Please paste the iCal feed URL from Canvas (it should start with https:// and end in .ics).")
		return
	}

	// Save the feed
	if err := b.store.UpsertFeed(ctx, userID, text); err != nil {
		log.Printf("upsert feed failed: %v", err)
		b.reply(msg.Chat.ID, "I could not save your feed. Please try again.")
		return
	}

	// Fetch and parse
	events, seeds, err := canvas.FetchAndDetect(text)
	if err != nil {
		log.Printf("canvas parse failed: %v", err)
		b.reply(msg.Chat.ID, "I saved your URL, but I could not parse the Canvas feed. The link might be invalid. Please check and try again with /start.")
		return
	}

	// Seed default settings (all Active)
	if err := b.store.SeedDefaultSettings(ctx, userID, seeds); err != nil {
		log.Printf("seed default settings failed: %v", err)
		b.reply(msg.Chat.ID, "I parsed your courses, but failed to save defaults. Please try again with /start.")
		return
	}

	courseCount := uniqueCourseCount(events)
	eventCount := len(events)

	// Update onboarding status
	if err := b.store.SetOnboardingStatus(ctx, userID, OnboardingGooglePrompt); err != nil {
		log.Printf("set onboarding status failed: %v", err)
	}

	// Ask about Google Calendar
	msgText := fmt.Sprintf("✅ Canvas connected! I found %d courses with %d upcoming events.\n\nNow, would you like to connect Google Calendar?\n\nYour assignments will automatically appear in your calendar. If you skip this, you can still receive notifications here in Telegram.", courseCount, eventCount)
	replyMsg := tgbotapi.NewMessage(msg.Chat.ID, msgText)
	replyMsg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Yes, connect Google", CBPrefixOnboardGoogleYes),
			tgbotapi.NewInlineKeyboardButtonData("🚫 No, skip for now", CBPrefixOnboardGoogleNo),
		),
	)
	if _, err := b.api.Send(replyMsg); err != nil {
		log.Printf("telegram send failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Onboarding: Google Calendar prompt response
// ---------------------------------------------------------------------------

func (b *Bot) handleOnboardingGoogleYes(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64) {
	b.answerCallback(cq.ID, "")

	// Check if already connected
	connected, err := b.oauthServer.IsConnected(ctx, userID)
	if err != nil {
		log.Printf("google status check failed: %v", err)
		b.reply(cq.Message.Chat.ID, "I could not check Google status right now.")
		return
	}
	if connected {
		b.reply(cq.Message.Chat.ID, "Your Google Calendar is already connected!")
		b.startCourseSetup(ctx, cq.Message.Chat.ID, userID)
		return
	}

	// Generate auth URL
	authURL, err := b.oauthServer.AuthURL(ctx, userID)
	if err != nil {
		log.Printf("google auth url generation failed: %v", err)
		b.reply(cq.Message.Chat.ID, "I could not get a Google connect link right now.")
		return
	}

	// Send clickable link and WAIT for OAuth to complete
	b.pendingOAuth[userID] = cq.Message.Chat.ID
	b.reply(cq.Message.Chat.ID, fmt.Sprintf("🔗 Click here to connect Google Calendar:\n%s\n\n⚠️ After you authorize, come back here and I'll continue setting up your courses!", authURL))
}

func (b *Bot) handleOnboardingGoogleNo(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64) {
	b.answerCallback(cq.ID, "")
	b.reply(cq.Message.Chat.ID, "No problem! You can still receive notifications here in Telegram.\nYou can connect Google Calendar anytime from /settings.\n\nSince Google Calendar isn't connected, all courses will be set to 📋 Review mode (you'll get a notification to review before adding). You can change this later with /settings.")

	b.sendModesGuide(ctx, cq.Message.Chat.ID, userID)
}

// ---------------------------------------------------------------------------
// Onboarding: One-by-one course setup
// ---------------------------------------------------------------------------

// onboardingState tracks where the user is in the course setup flow.
// Stored in-memory since it's transient per-session; survives via DB status.
type onboardingProgress struct {
	CourseIndex int
	TypeIndex   int
	CourseIDs   []string
	Types       []string
}

// in-memory map for course setup progress (lost on restart, user just redoes setup)
var onboardingProgressMap = map[int64]*onboardingProgress{}

// sendModesGuide explains the sync modes and then starts the course setup prompts.
func (b *Bot) sendModesGuide(ctx context.Context, chatID int64, userID int64) {
	googleConnected, _ := b.oauthServer.IsConnected(ctx, userID)

	var guide string
	if googleConnected {
		guide = `📚 Sync Modes Guide

Each course has event types (assignments, quizzes, etc.). You can set how each type behaves:

🔇 Quiet Sync — Events are automatically added to your Google Calendar. No Telegram notification.
🔔 Notify & Sync — Events are added to your Google Calendar AND you get a Telegram notification.
📋 Review — You'll receive a notification here asking if you want to add it to your calendar.
🚫 Ignore — Events are filtered out and ignored.

Let's set up your preferences now!`
	} else {
		guide = `📚 Sync Modes Guide

Each course has event types (assignments, quizzes, etc.). You can set how each type behaves:

📋 Review — You'll receive a notification here asking if you want to add it to your calendar.
🚫 Ignore — Events are filtered out and ignored.

🔇 Quiet Sync and 🔔 Notify & Sync are only available after connecting Google Calendar from /settings.

Let's set up your preferences now!`
	}

	b.reply(chatID, guide)
	b.startCourseSetup(ctx, chatID, userID)
}

func (b *Bot) startCourseSetup(ctx context.Context, chatID int64, userID int64) {
	courseIDs, err := b.store.ListUserCourseIDs(ctx, userID)
	if err != nil || len(courseIDs) == 0 {
		log.Printf("list course ids failed or empty: %v", err)
		b.finishOnboarding(ctx, chatID, userID)
		return
	}

	// Get types for the first course
	types, err := b.store.ListCourseTypes(ctx, userID, courseIDs[0])
	if err != nil || len(types) == 0 {
		log.Printf("list course types failed or empty: %v", err)
		b.finishOnboarding(ctx, chatID, userID)
		return
	}

	onboardingProgressMap[userID] = &onboardingProgress{
		CourseIndex: 0,
		TypeIndex:   0,
		CourseIDs:   courseIDs,
		Types:       types,
	}

	b.sendCourseSetupPrompt(ctx, chatID, userID)
}

func (b *Bot) sendCourseSetupPrompt(ctx context.Context, chatID int64, userID int64) {
	prog, ok := onboardingProgressMap[userID]
	if !ok || prog.CourseIndex >= len(prog.CourseIDs) {
		b.finishOnboarding(ctx, chatID, userID)
		return
	}

	courseID := prog.CourseIDs[prog.CourseIndex]

	// Refresh types list for current course
	types, err := b.store.ListCourseTypes(ctx, userID, courseID)
	if err != nil || len(types) == 0 {
		// Skip this course, move to next
		prog.CourseIndex++
		prog.TypeIndex = 0
		if prog.CourseIndex < len(prog.CourseIDs) {
			nextTypes, err := b.store.ListCourseTypes(ctx, userID, prog.CourseIDs[prog.CourseIndex])
			if err == nil {
				prog.Types = nextTypes
			}
		}
		b.sendCourseSetupPrompt(ctx, chatID, userID)
		return
	}
	prog.Types = types

	if prog.TypeIndex >= len(prog.Types) {
		// Move to next course
		prog.CourseIndex++
		prog.TypeIndex = 0
		if prog.CourseIndex >= len(prog.CourseIDs) {
			b.finishOnboarding(ctx, chatID, userID)
			return
		}
		nextTypes, err := b.store.ListCourseTypes(ctx, userID, prog.CourseIDs[prog.CourseIndex])
		if err != nil || len(nextTypes) == 0 {
			b.sendCourseSetupPrompt(ctx, chatID, userID)
			return
		}
		prog.Types = nextTypes
		b.sendCourseSetupPrompt(ctx, chatID, userID)
		return
	}

	assignmentType := prog.Types[prog.TypeIndex]

	// Calculate overall progress
	overallIndex := 0
	for i := 0; i < prog.CourseIndex; i++ {
		typesForCourse, _ := b.store.ListCourseTypes(ctx, userID, prog.CourseIDs[i])
		overallIndex += len(typesForCourse)
	}
	overallIndex += prog.TypeIndex + 1

	// Count total
	totalItems := 0
	for _, cid := range prog.CourseIDs {
		typesForCourse, _ := b.store.ListCourseTypes(ctx, userID, cid)
		totalItems += len(typesForCourse)
	}

	msg := fmt.Sprintf("📚 Step %d/%d: %s\nHow should I handle %s for this course?",
		overallIndex, totalItems, courseID, typeLabel(assignmentType))

	// Only show Quiet Sync and Notify & Sync if Google Calendar is connected
	googleConnected, _ := b.oauthServer.IsConnected(ctx, userID)

	var row []tgbotapi.InlineKeyboardButton
	if googleConnected {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("🔇 Quiet", fmt.Sprintf("%s%s|%s|quiet", CBPrefixOnboardMode, courseID, assignmentType)))
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("🔔 Notify", fmt.Sprintf("%s%s|%s|notify", CBPrefixOnboardMode, courseID, assignmentType)))
	}
	row = append(row,
		tgbotapi.NewInlineKeyboardButtonData("📋 Review", fmt.Sprintf("%s%s|%s|review", CBPrefixOnboardMode, courseID, assignmentType)),
		tgbotapi.NewInlineKeyboardButtonData("🚫 Ignore", fmt.Sprintf("%s%s|%s|ignore", CBPrefixOnboardMode, courseID, assignmentType)),
	)

	replyMsg := tgbotapi.NewMessage(chatID, msg)
	replyMsg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(row...),
	)
	if _, err := b.api.Send(replyMsg); err != nil {
		log.Printf("telegram send failed: %v", err)
	}
}

func (b *Bot) handleOnboardingMode(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, data string) {
	// data format: ob_mode|COURSE_ID|TYPE|MODE
	parts := strings.Split(data, "|")
	if len(parts) != 4 {
		b.answerCallback(cq.ID, "Invalid")
		return
	}
	courseID := parts[1]
	assignmentType := parts[2]
	mode := parts[3]

	if mode != store.ModeQuiet && mode != store.ModeNotify && mode != store.ModeReview && mode != store.ModeIgnore {
		b.answerCallback(cq.ID, "Invalid")
		return
	}

	if err := b.store.SetCourseTypeMode(ctx, userID, courseID, assignmentType, mode); err != nil {
		log.Printf("set mode failed: %v", err)
		b.answerCallback(cq.ID, "Failed")
		return
	}

	b.answerCallback(cq.ID, "Saved ✅")

	// If user selected a Cat 1 mode (quiet/notify), ask about syncing past assignments
	if isCat1(mode) {
		b.askSyncPast(ctx, cq.Message.Chat.ID, cq.Message.MessageID, userID, courseID, assignmentType, mode, true)
		return
	}

	// Advance to next item
	prog, ok := onboardingProgressMap[userID]
	if !ok {
		b.finishOnboarding(ctx, cq.Message.Chat.ID, userID)
		return
	}

	prog.TypeIndex++
	b.sendCourseSetupPrompt(ctx, cq.Message.Chat.ID, userID)
}

// handleOnboardingSyncPast processes the sync-past selection during onboarding.
// The mode was already saved by handleOnboardingMode before the sync-past prompt was shown.
// This function processes the past-event sync option and advances to the next onboarding item.
func (b *Bot) handleOnboardingSyncPast(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, courseID, assignmentType, targetMode, option string) {
	b.answerCallback(cq.ID, "")

	// If "future only", no past events to sync — just advance
	if option == "future" {
		prog, ok := onboardingProgressMap[userID]
		if !ok {
			b.finishOnboarding(ctx, cq.Message.Chat.ID, userID)
			return
		}
		prog.TypeIndex++
		b.sendCourseSetupPrompt(ctx, cq.Message.Chat.ID, userID)
		return
	}

	// Calculate the cutoff time
	now := time.Now()
	var cutoff time.Time
	switch option {
	case "7d":
		cutoff = now.AddDate(0, 0, -7)
	case "30d":
		cutoff = now.AddDate(0, 0, -30)
	case "all":
		cutoff = time.Time{} // zero time = no cutoff
	}

	// Fetch the feed to get past events
	feed, err := b.store.GetFeed(ctx, userID)
	if err != nil || feed == nil {
		// Can't fetch feed — just advance onboarding
		prog, ok := onboardingProgressMap[userID]
		if !ok {
			b.finishOnboarding(ctx, cq.Message.Chat.ID, userID)
			return
		}
		prog.TypeIndex++
		b.sendCourseSetupPrompt(ctx, cq.Message.Chat.ID, userID)
		return
	}

	events, _, err := canvas.FetchAndDetect(feed.ICalURL)
	if err != nil {
		prog, ok := onboardingProgressMap[userID]
		if !ok {
			b.finishOnboarding(ctx, cq.Message.Chat.ID, userID)
			return
		}
		prog.TypeIndex++
		b.sendCourseSetupPrompt(ctx, cq.Message.Chat.ID, userID)
		return
	}

	// Filter past events matching this course and type
	var pastEvents []canvas.Event
	for _, ev := range events {
		if ev.Course != courseID || ev.Type != assignmentType || ev.DueAt == nil {
			continue
		}
		if ev.DueAt.Before(now) {
			if option != "all" && ev.DueAt.Before(cutoff) {
				continue
			}
			pastEvents = append(pastEvents, ev)
		}
	}

	// Sync past events to Google Calendar (mode is already saved as Cat 1)
	synced := 0
	for _, ev := range pastEvents {
		calendarID, eventID, err := b.google.CreateEventForTelegramUser(
			ctx, userID, ev.Title, *ev.DueAt,
			fmt.Sprintf("CanvasLink onboarding-sync\nCourse: %s\nType: %s", ev.Course, ev.Type),
		)
		if err != nil {
			log.Printf("onboarding sync past event failed: %v", err)
			continue
		}
		_, err = b.store.UpsertSyncedEvent(ctx, store.SyncedEventInput{
			TelegramUserID:   userID,
			CourseID:         ev.Course,
			AssignmentType:   ev.Type,
			CanvasEventUID:   ev.UID,
			CanvasTitle:      ev.Title,
			CanvasDueAt:      *ev.DueAt,
			GoogleCalendarID: calendarID,
			GoogleEventID:    eventID,
			CanvasDTStamp:    ev.DTStamp,
			CanvasSequence:   ev.Sequence,
		})
		if err != nil {
			log.Printf("onboarding sync past event record failed: %v", err)
			continue
		}
		synced++
	}

	if synced > 0 {
		b.reply(cq.Message.Chat.ID, fmt.Sprintf("✅ Synced %d past %s to your Google Calendar.", synced, typeLabel(assignmentType)))
	}

	// Advance to next onboarding item
	prog, ok := onboardingProgressMap[userID]
	if !ok {
		b.finishOnboarding(ctx, cq.Message.Chat.ID, userID)
		return
	}
	prog.TypeIndex++
	b.sendCourseSetupPrompt(ctx, cq.Message.Chat.ID, userID)
}

func (b *Bot) finishOnboarding(ctx context.Context, chatID int64, userID int64) {
	delete(onboardingProgressMap, userID)

	// Clear onboarding status
	if err := b.store.SetOnboardingStatus(ctx, userID, ""); err != nil {
		log.Printf("clear onboarding status failed: %v", err)
	}

	// Get summary
	courses, err := b.store.ListUserCourses(ctx, userID)
	courseCount := len(courses)
	if err != nil {
		courseCount = 0
	}

	b.reply(chatID, fmt.Sprintf(`✅ All set! CanvasLink is now monitoring your Canvas feed.

Here's a summary:
📚 %d courses configured
🔇 Quiet Sync: events go straight to your calendar, no notifications
🔔 Notify & Sync: events go to calendar + you get a notification
📋 Review: you'll get a notification to review before adding
🚫 Ignore: events are filtered out

You can change these anytime with /settings.

I'll check for new events every hour.`, courseCount))
}

// ---------------------------------------------------------------------------
// Confirmation dialog helper
// ---------------------------------------------------------------------------

func (b *Bot) sendConfirmDialog(ctx context.Context, chatID int64, action string, text string) {
	confirmMsg := tgbotapi.NewMessage(chatID, text)
	confirmMsg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Yes", CBPrefixConfirm+action),
			tgbotapi.NewInlineKeyboardButtonData("🔙 Cancel", CBPrefixCancel),
		),
	)
	if _, err := b.api.Send(confirmMsg); err != nil {
		log.Printf("telegram send failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Confirmation handlers
// ---------------------------------------------------------------------------

func (b *Bot) handleConfirm(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, action string) {
	b.answerCallback(cq.ID, "")

	switch action {
	case "disconnect_canvas":
		if err := b.store.DeleteAllCourseSettings(ctx, userID); err != nil {
			log.Printf("delete course settings failed: %v", err)
		}
		if err := b.store.DeleteAllSyncedEvents(ctx, userID); err != nil {
			log.Printf("delete synced events failed: %v", err)
		}
		if err := b.store.DeleteAllPendingActions(ctx, userID); err != nil {
			log.Printf("delete pending actions failed: %v", err)
		}
		if err := b.store.DeleteFeed(ctx, userID); err != nil {
			log.Printf("delete feed failed: %v", err)
		}
		if err := b.store.SetOnboardingStatus(ctx, userID, ""); err != nil {
			log.Printf("clear onboarding status failed: %v", err)
		}
		b.reply(cq.Message.Chat.ID, "✅ Canvas disconnected. Your Google Calendar events remain unchanged.\n\nSend /start to reconnect.")

	case "disconnect_google":
		if err := b.store.DeleteGoogleToken(ctx, userID); err != nil {
			log.Printf("delete google token failed: %v", err)
		}
		b.reply(cq.Message.Chat.ID, "✅ Google Calendar disconnected. Your existing events remain in your calendar.")

	case "reset":
		// Delete CanvasLink events from Google Calendar
		if b.google != nil {
			syncedEvents, err := b.store.ListSyncedEvents(ctx, userID)
			if err == nil {
				for _, se := range syncedEvents {
					if se.GoogleEventID != "" {
						// Attempt to delete the event from Google Calendar
						if err := b.google.DeleteEvent(ctx, userID, se.GoogleCalendarID, se.GoogleEventID); err != nil {
							log.Printf("delete google event failed: %v", err)
						}
					}
				}
			}
		}

		if err := b.store.DeleteAllCourseSettings(ctx, userID); err != nil {
			log.Printf("delete course settings failed: %v", err)
		}
		if err := b.store.DeleteAllSyncedEvents(ctx, userID); err != nil {
			log.Printf("delete synced events failed: %v", err)
		}
		if err := b.store.DeleteAllPendingActions(ctx, userID); err != nil {
			log.Printf("delete pending actions failed: %v", err)
		}
		if err := b.store.DeleteFeed(ctx, userID); err != nil {
			log.Printf("delete feed failed: %v", err)
		}
		if err := b.store.DeleteGoogleToken(ctx, userID); err != nil {
			log.Printf("delete google token failed: %v", err)
		}
		if err := b.store.DeleteTelegramAccount(ctx, userID); err != nil {
			log.Printf("delete telegram account failed: %v", err)
		}
		b.reply(cq.Message.Chat.ID, "✅ All data has been reset. CanvasLink events have been removed from your Google Calendar.\n\nSend /start to begin again.")
	}
}

// ---------------------------------------------------------------------------
// Callback handler
// ---------------------------------------------------------------------------

func (b *Bot) handleCallback(ctx context.Context, cq *tgbotapi.CallbackQuery) {
	if cq == nil || cq.From == nil || cq.Message == nil {
		return
	}
	userID := int64(cq.From.ID)
	data := strings.TrimSpace(cq.Data)

	switch {
	case data == CBPrefixOnboardGoogleYes:
		b.handleOnboardingGoogleYes(ctx, cq, userID)
	case data == CBPrefixOnboardGoogleNo:
		b.handleOnboardingGoogleNo(ctx, cq, userID)
	case data == CBPrefixOnboardDone:
		b.answerCallback(cq.ID, "")
		b.finishOnboarding(ctx, cq.Message.Chat.ID, userID)
	case strings.HasPrefix(data, CBPrefixOnboardMode):
		b.handleOnboardingMode(ctx, cq, userID, data)
	case strings.HasPrefix(data, "ob_sync_past|"):
		// Onboarding sync-past selection: ob_sync_past|COURSE_ID|TYPE|TARGET_MODE|OPTION
		parts := strings.Split(data, "|")
		if len(parts) == 5 {
			courseID, assignmentType, targetMode, option := parts[1], parts[2], parts[3], parts[4]
			b.handleOnboardingSyncPast(ctx, cq, userID, courseID, assignmentType, targetMode, option)
		} else {
			b.answerCallback(cq.ID, "Invalid")
		}
	case strings.HasPrefix(data, CBPrefixSettingsHub):
		action := strings.TrimPrefix(data, CBPrefixSettingsHub)
		if action == "main" {
			// Edit back to the status dashboard
			statusText := b.buildStatusText(ctx, userID)
			edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, statusText)
			edit.ParseMode = "Markdown"
			edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
				InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData("🔗 Integrations", CBPrefixSettingsHub+"integrations"),
					),
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData("⚙️ Preferences", CBPrefixSettingsHub+"preferences"),
					),
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData("🔄 Sync Now", CBPrefixSyncNow),
					),
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData("❌ Close", CBPrefixClose),
					),
				},
			}
			if _, err := b.api.Send(edit); err != nil {
				log.Printf("telegram edit failed: %v", err)
			}
		} else {
			b.handleSettingsCallback(ctx, cq, userID, action)
		}
	case strings.HasPrefix(data, CBPrefixIntegrations):
		action := strings.TrimPrefix(data, CBPrefixIntegrations)
		b.handleIntegrationsCallback(ctx, cq, userID, action)
	case strings.HasPrefix(data, CBPrefixPreferences):
		action := strings.TrimPrefix(data, CBPrefixPreferences)
		if action == "main" {
			b.showPreferencesMenu(ctx, cq, userID)
		} else {
			b.handlePreferencesCallback(ctx, cq, userID, action)
		}
	case data == CBPrefixSyncNow:
		b.handleSyncNow(ctx, cq, userID)
	case data == CBPrefixClose:
		b.answerCallback(cq.ID, "Closed")
		// Delete the settings message
		del := tgbotapi.NewDeleteMessage(cq.Message.Chat.ID, cq.Message.MessageID)
		if _, err := b.api.Request(del); err != nil {
			log.Printf("delete message failed: %v", err)
		}
	case strings.HasPrefix(data, CBPrefixConfirm):
		action := strings.TrimPrefix(data, CBPrefixConfirm)
		b.handleConfirm(ctx, cq, userID, action)
	case data == CBPrefixCancel:
		b.answerCallback(cq.ID, "Cancelled")
	case strings.HasPrefix(data, CBPrefixNoop):
		b.answerCallback(cq.ID, "")
	case strings.HasPrefix(data, CBPrefixInterval):
		interval := strings.TrimPrefix(data, CBPrefixInterval)
		b.handleIntervalCallback(ctx, cq, userID, interval)
	case strings.HasPrefix(data, CBPrefixCourse):
		courseID := strings.TrimPrefix(data, CBPrefixCourse)
		b.answerCallback(cq.ID, "")
		b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
	case data == CBPrefixModules:
		b.answerCallback(cq.ID, "")
		b.editModulesMenu(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID)
	case strings.HasPrefix(data, CBPrefixModeSelect):
		// User tapped a type button in the course matrix — show mode selection prompt.
		parts := strings.Split(data, "|")
		if len(parts) != 3 {
			b.answerCallback(cq.ID, "Invalid")
			return
		}
		courseID, assignmentType := parts[1], parts[2]

		// Check if Google is connected to decide whether to show Quiet and Notify.
		hasGoogle, _ := b.store.HasGoogleToken(ctx, userID)

		text := fmt.Sprintf("Select mode for *%s* in *%s*:", typeLabel(assignmentType), courseID)
		edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, text)
		edit.ParseMode = "Markdown"

		var rows [][]tgbotapi.InlineKeyboardButton
		if hasGoogle {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔇 Quiet Sync", CBPrefixMode+courseID+"|"+assignmentType+"|"+store.ModeQuiet),
			))
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔔 Notify & Sync", CBPrefixMode+courseID+"|"+assignmentType+"|"+store.ModeNotify),
			))
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Review", CBPrefixMode+courseID+"|"+assignmentType+"|"+store.ModeReview),
		))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🚫 Ignore", CBPrefixMode+courseID+"|"+assignmentType+"|"+store.ModeIgnore),
		))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", CBPrefixCourse+courseID),
		))
		edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}

		b.answerCallback(cq.ID, "")
		if _, err := b.api.Send(edit); err != nil {
			log.Printf("telegram edit failed: %v", err)
		}

	case strings.HasPrefix(data, CBPrefixMode):
		parts := strings.Split(data, "|")
		if len(parts) != 4 {
			b.answerCallback(cq.ID, "Invalid")
			return
		}
		courseID, assignmentType, newMode := parts[1], parts[2], parts[3]
		if newMode != store.ModeQuiet && newMode != store.ModeNotify && newMode != store.ModeReview && newMode != store.ModeIgnore {
			b.answerCallback(cq.ID, "Invalid")
			return
		}

		// Get current mode to detect transitions
		currentMode, _ := b.store.GetCourseTypeMode(ctx, userID, courseID, assignmentType)

		// Skip transition prompts during onboarding
		_, inOnboarding := onboardingProgressMap[userID]
		if !inOnboarding {
			// Check if we need to prompt for the transition
			promptType := b.transitionPrompt(ctx, userID, courseID, assignmentType, currentMode, newMode)
			switch promptType {
			case "remove_cal":
				// Cat 1 → Cat 2: ask about removing from calendar
				b.answerCallback(cq.ID, "")
				text := fmt.Sprintf("Switching to Ignore for *%s* in *%s*.\n\nWould you like to remove existing events from your Google Calendar?", typeLabel(assignmentType), courseID)
				edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, text)
				edit.ParseMode = "Markdown"
				edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
					InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
						{
							tgbotapi.NewInlineKeyboardButtonData("✅ Yes, remove them", CBPrefixRemoveCal+courseID+"|"+assignmentType+"|"+newMode+"|yes"),
							tgbotapi.NewInlineKeyboardButtonData("🚫 No, keep them", CBPrefixRemoveCal+courseID+"|"+assignmentType+"|"+newMode+"|no"),
						},
						{
							tgbotapi.NewInlineKeyboardButtonData("🔙 Cancel", CBPrefixCourse+courseID),
						},
					},
				}
				if _, err := b.api.Send(edit); err != nil {
					log.Printf("telegram edit failed: %v", err)
				}
				return

			case "sync_past":
				// Cat 2 → Cat 1: ask about syncing past assignments
				b.answerCallback(cq.ID, "")
				b.askSyncPast(ctx, cq.Message.Chat.ID, cq.Message.MessageID, userID, courseID, assignmentType, newMode, false)
				return

			case "sync_past_review":
				// Cat 2 → Review: ask about syncing past, then check for historical items
				b.answerCallback(cq.ID, "")
				b.askSyncPast(ctx, cq.Message.Chat.ID, cq.Message.MessageID, userID, courseID, assignmentType, newMode, false)
				return
			}
		}

		// No transition prompt needed — save directly
		b.applyModeChange(ctx, cq, userID, courseID, assignmentType, newMode)

	case strings.HasPrefix(data, CBPrefixNewCourseMode):
		// New course mode selection: new_course_mode|COURSE_ID|TYPE|TYPE_INDEX|TOTAL_TYPES|MODE
		parts := strings.Split(data, "|")
		if len(parts) != 6 {
			b.answerCallback(cq.ID, "Invalid")
			return
		}
		courseID, assignmentType := parts[1], parts[2]
		typeIndex, _ := strconv.Atoi(parts[3])
		totalTypes, _ := strconv.Atoi(parts[4])
		mode := parts[5]

		if mode != store.ModeQuiet && mode != store.ModeNotify && mode != store.ModeReview && mode != store.ModeIgnore {
			b.answerCallback(cq.ID, "Invalid")
			return
		}

		// Save the mode
		if err := b.store.SetCourseTypeMode(ctx, userID, courseID, assignmentType, mode); err != nil {
			log.Printf("set mode failed: %v", err)
			b.answerCallback(cq.ID, "Failed")
			return
		}

		b.answerCallback(cq.ID, "Saved ✅")

		// If Cat 1 mode, ask about syncing past assignments
		if isCat1(mode) {
			// For new course config from worker, we use a simplified sync-past prompt
			// that doesn't rely on onboarding progress map
			b.askNewCourseSyncPast(ctx, cq.Message.Chat.ID, cq.Message.MessageID, userID, courseID, assignmentType, mode, typeIndex, totalTypes)
			return
		}

		// Check if there are more types to configure
		if typeIndex+1 < totalTypes {
			// Send the next prompt — we need to get the next type
			// The types are ordered alphabetically, so we can look them up
			types, err := b.store.ListCourseTypes(ctx, userID, courseID)
			if err == nil && typeIndex+1 < len(types) {
				nextType := types[typeIndex+1]
				b.sendNewCoursePrompt(ctx, cq.Message.Chat.ID, userID, courseID, nextType, typeIndex+1, totalTypes)
				return
			}
		}

		// All types configured for this course
		b.reply(cq.Message.Chat.ID, fmt.Sprintf("✅ All set for *%s*! I'll start monitoring this course.", courseID))

	case strings.HasPrefix(data, "new_course_sync|"):
		// New course sync-past selection: new_course_sync|COURSE_ID|TYPE|TYPE_INDEX|TOTAL_TYPES|TARGET_MODE|OPTION
		parts := strings.Split(data, "|")
		if len(parts) != 7 {
			b.answerCallback(cq.ID, "Invalid")
			return
		}
		courseID, assignmentType := parts[1], parts[2]
		typeIndex, _ := strconv.Atoi(parts[3])
		totalTypes, _ := strconv.Atoi(parts[4])
		targetMode, option := parts[5], parts[6]
		b.handleNewCourseSyncPast(ctx, cq, userID, courseID, assignmentType, typeIndex, totalTypes, targetMode, option)

	case strings.HasPrefix(data, CBPrefixSyncPast):
		// User selected a sync-past option: sync_past|COURSE_ID|TYPE|TARGET_MODE|OPTION
		parts := strings.Split(data, "|")
		if len(parts) != 5 {
			b.answerCallback(cq.ID, "Invalid")
			return
		}
		courseID, assignmentType, targetMode, option := parts[1], parts[2], parts[3], parts[4]
		b.handleSyncPastOption(ctx, cq, userID, courseID, assignmentType, targetMode, option)

	case strings.HasPrefix(data, CBPrefixRemoveCal):
		// User responded to remove-from-calendar prompt: remove_cal|COURSE_ID|TYPE|TARGET_MODE|ACTION
		parts := strings.Split(data, "|")
		if len(parts) != 5 {
			b.answerCallback(cq.ID, "Invalid")
			return
		}
		courseID, assignmentType, targetMode, action := parts[1], parts[2], parts[3], parts[4]
		b.handleRemoveCalOption(ctx, cq, userID, courseID, assignmentType, targetMode, action)

	case strings.HasPrefix(data, CBPrefixHistorical):
		// User responded to historical items prompt: historical|COURSE_ID|TYPE|TARGET_MODE|OPTION
		parts := strings.Split(data, "|")
		if len(parts) != 5 {
			b.answerCallback(cq.ID, "Invalid")
			return
		}
		courseID, assignmentType, targetMode, option := parts[1], parts[2], parts[3], parts[4]
		b.handleHistoricalOption(ctx, cq, userID, courseID, assignmentType, targetMode, option)
	case strings.HasPrefix(data, CBPrefixPending):
		b.handlePendingAction(ctx, cq, userID, data)
	default:
		b.answerCallback(cq.ID, "Unknown")
	}
}

// ---------------------------------------------------------------------------
// Pending action handler (Active mode confirmation)
// ---------------------------------------------------------------------------

func (b *Bot) handlePendingAction(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, data string) {
	parts := strings.Split(data, "|")
	if len(parts) != 3 {
		b.answerCallback(cq.ID, "Invalid action")
		return
	}
	action := parts[1]
	pendingID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		b.answerCallback(cq.ID, "Invalid action")
		return
	}

	pending, err := b.store.GetPendingAction(ctx, userID, pendingID)
	if err != nil || pending == nil {
		b.answerCallback(cq.ID, "Not found")
		return
	}

	switch action {
	case "add":
		calendarID, eventID, err := b.google.CreateEventForTelegramUser(
			ctx,
			userID,
			pending.CanvasTitle,
			pending.CanvasDueAt,
			fmt.Sprintf("CanvasLink manual add\nCourse: %s\nType: %s", pending.CourseID, pending.AssignmentType),
		)
		if errors.Is(err, canvasGoogle.ErrGoogleNotConnected) {
			b.answerCallback(cq.ID, "Connect Google first")
			return
		}
		if errors.Is(err, canvasGoogle.ErrGoogleNotConfigured) {
			b.answerCallback(cq.ID, "Google not configured")
			return
		}
		if err != nil {
			log.Printf("google add failed: %v", err)
			b.answerCallback(cq.ID, "Google add failed")
			return
		}

		if err := b.store.MarkPendingStatus(ctx, userID, pendingID, store.PendingStatusAdded); err != nil {
			b.answerCallback(cq.ID, "Failed")
			return
		}
		_, err = b.store.UpsertSyncedEvent(ctx, store.SyncedEventInput{
			TelegramUserID:   pending.TelegramUserID,
			CourseID:         pending.CourseID,
			AssignmentType:   pending.AssignmentType,
			CanvasEventUID:   pending.CanvasEventUID,
			CanvasTitle:      pending.CanvasTitle,
			CanvasDueAt:      pending.CanvasDueAt,
			GoogleCalendarID: calendarID,
			GoogleEventID:    eventID,
		})
		if err != nil {
			log.Printf("upsert synced event from pending failed: %v", err)
		}
		b.answerCallback(cq.ID, "Added to calendar queue")
		b.reply(cq.Message.Chat.ID, "Added. I will sync this to calendar.")
	case "ignore":
		if err := b.store.MarkPendingStatus(ctx, userID, pendingID, store.PendingStatusIgnored); err != nil {
			b.answerCallback(cq.ID, "Failed")
			return
		}
		b.answerCallback(cq.ID, "Ignored")
		b.reply(cq.Message.Chat.ID, "Ignored. I will filter this item.")
	default:
		b.answerCallback(cq.ID, "Unknown action")
	}
}

// ---------------------------------------------------------------------------
// Course settings (matrix UI)
// ---------------------------------------------------------------------------

func (b *Bot) sendModulesMenu(ctx context.Context, userID int64, chatID int64) {
	courses, err := b.store.ListUserCourses(ctx, userID)
	if err != nil {
		log.Printf("list courses failed: %v", err)
		b.reply(chatID, "Could not load settings yet.")
		return
	}
	if len(courses) == 0 {
		b.reply(chatID, "No courses detected yet. Send /start to begin setup.")
		return
	}
	msg := tgbotapi.NewMessage(chatID, "CanvasLink Settings\n\nChoose a course:")
	keyboard := modulesKeyboard(courses)
	msg.ReplyMarkup = keyboard
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("telegram send failed: %v", err)
	}
}

func (b *Bot) editModulesMenu(ctx context.Context, userID int64, chatID int64, messageID int) {
	courses, err := b.store.ListUserCourses(ctx, userID)
	if err != nil {
		log.Printf("list courses failed: %v", err)
		return
	}
	msg := tgbotapi.NewEditMessageText(chatID, messageID, "CanvasLink Settings\n\nChoose a course:")
	msg.ReplyMarkup = modulesKeyboard(courses)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("telegram edit failed: %v", err)
	}
}

func (b *Bot) editCourseMatrix(ctx context.Context, userID int64, chatID int64, messageID int, courseID string) {
	settings, err := b.store.ListCourseSettings(ctx, userID, courseID)
	if err != nil || len(settings) == 0 {
		log.Printf("list course settings failed: %v", err)
		return
	}
	msg := buildSettingsMatrixMessage(courseID, settings)
	edit := tgbotapi.NewEditMessageText(chatID, messageID, msg.Text)
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = courseSettingsKeyboard(courseID, settings)
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("telegram edit failed: %v", err)
	}
}

func (b *Bot) answerCallback(callbackID string, text string) {
	cfg := tgbotapi.NewCallback(callbackID, text)
	if _, err := b.api.Request(cfg); err != nil {
		log.Printf("answer callback failed: %v", err)
	}
}

func (b *Bot) reply(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("telegram send failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Keyboard builders
// ---------------------------------------------------------------------------

func modulesKeyboard(courses []store.Course) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, c := range courses {
		label := c.CourseID
		if c.CourseName != "" && c.CourseName != c.CourseID {
			label = c.CourseName
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, CBPrefixCourse+c.CourseID),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 Back to Preferences", CBPrefixPreferences+"main"),
	))
	markup := tgbotapi.NewInlineKeyboardMarkup(rows...)
	return &markup
}

func buildSettingsMatrixMessage(courseID string, settings []store.CourseSetting) tgbotapi.MessageConfig {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📚 *%s*\n\n", courseID))
	sb.WriteString("Tap a button below to change the sync mode for each type.\n\n")
	for _, s := range settings {
		emoji := modeEmoji(s.Mode)
		label := modeLabel(s.Mode)
		sb.WriteString(fmt.Sprintf("%s *%s* → %s %s\n", emoji, typeLabel(s.AssignmentType), emoji, label))
	}
	sb.WriteString("\nSelect a type to change its mode:")
	msg := tgbotapi.NewMessage(0, sb.String())
	msg.ParseMode = "Markdown"
	return msg
}

func courseSettingsKeyboard(courseID string, settings []store.CourseSetting) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, s := range settings {
		label := fmt.Sprintf("%s %s", typeLabel(s.AssignmentType), modeEmoji(s.Mode))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, CBPrefixModeSelect+courseID+"|"+s.AssignmentType),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 Back to Courses", CBPrefixModules),
	))
	markup := tgbotapi.NewInlineKeyboardMarkup(rows...)
	return &markup
}

func modeEmoji(mode string) string {
	switch mode {
	case store.ModeQuiet:
		return "🔇"
	case store.ModeNotify:
		return "🔔"
	case store.ModeReview:
		return "📋"
	case store.ModeIgnore:
		return "🚫"
	default:
		return "❓"
	}
}

func modeLabel(mode string) string {
	switch mode {
	case store.ModeQuiet:
		return "Quiet Sync"
	case store.ModeNotify:
		return "Notify & Sync"
	case store.ModeReview:
		return "Review"
	case store.ModeIgnore:
		return "Ignore"
	default:
		return mode
	}
}

// isCat1 returns true if the mode syncs to Google Calendar (Quiet Sync or Notify & Sync).
func isCat1(mode string) bool {
	return mode == store.ModeQuiet || mode == store.ModeNotify
}

// isCat2 returns true if the mode does not sync to Google Calendar (Ignore).
func isCat2(mode string) bool {
	return mode == store.ModeIgnore
}

// applyModeChange saves the mode and refreshes the course matrix.
func (b *Bot) applyModeChange(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, courseID, assignmentType, mode string) {
	if err := b.store.SetCourseTypeMode(ctx, userID, courseID, assignmentType, mode); err != nil {
		log.Printf("set mode failed: %v", err)
		b.answerCallback(cq.ID, "Failed")
		return
	}
	b.answerCallback(cq.ID, "Updated")
	b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
}

// transitionPrompt checks if a mode transition requires a prompt.
// Returns a non-empty string describing the prompt type if one is needed.
// The caller should NOT save the mode yet — wait for user response.
func (b *Bot) transitionPrompt(ctx context.Context, userID int64, courseID, assignmentType, currentMode, newMode string) string {
	// No change — no prompt
	if currentMode == newMode {
		return ""
	}

	// Cat 1 → Cat 2 (quiet/notify → ignore): ask about removing from calendar
	if isCat1(currentMode) && isCat2(newMode) {
		return "remove_cal"
	}

	// Cat 2 → Cat 1 (ignore → quiet/notify): ask about syncing past assignments
	if isCat2(currentMode) && isCat1(newMode) {
		return "sync_past"
	}

	// Cat 2 → Review (ignore → review): ask about syncing past assignments
	if isCat2(currentMode) && newMode == store.ModeReview {
		return "sync_past_review"
	}

	// All other transitions: no prompt needed
	return ""
}

// askSyncPast shows the sync-past-assignments prompt with time range options.
// If isOnboarding is true, the callback advances onboarding after selection.
func (b *Bot) askSyncPast(ctx context.Context, chatID int64, messageID int, userID int64, courseID, assignmentType, targetMode string, isOnboarding bool) {
	prefix := CBPrefixSyncPast
	if isOnboarding {
		prefix = "ob_sync_past|"
	}

	text := fmt.Sprintf("Would you like to sync past %s for *%s* to your Google Calendar?", typeLabel(assignmentType), courseID)
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{
				tgbotapi.NewInlineKeyboardButtonData("🔮 Future only", prefix+courseID+"|"+assignmentType+"|"+targetMode+"|future"),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("📅 Past 7 days", prefix+courseID+"|"+assignmentType+"|"+targetMode+"|7d"),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("📅 Past 30 days", prefix+courseID+"|"+assignmentType+"|"+targetMode+"|30d"),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("📚 Entire course", prefix+courseID+"|"+assignmentType+"|"+targetMode+"|all"),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("🔙 Cancel", CBPrefixCourse+courseID),
			},
		},
	}
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("telegram edit failed: %v", err)
	}
}

// sendNewCoursePrompt sends a configuration prompt for a new course type (from worker detection).
// This is used when the worker detects a new course and the user taps a callback button.
func (b *Bot) sendNewCoursePrompt(ctx context.Context, chatID int64, userID int64, courseID, assignmentType string, typeIndex, totalTypes int) {
	googleConnected, _ := b.oauthServer.IsConnected(ctx, userID)

	text := fmt.Sprintf("📚 *%s* — Step %d/%d\nHow should I handle *%s*?",
		courseID, typeIndex+1, totalTypes, typeLabel(assignmentType))

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"

	baseData := fmt.Sprintf("new_course_mode|%s|%s|%d|%d|", courseID, assignmentType, typeIndex, totalTypes)

	var row []tgbotapi.InlineKeyboardButton
	if googleConnected {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("🔇 Quiet", baseData+"quiet"))
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("🔔 Notify", baseData+"notify"))
	}
	row = append(row,
		tgbotapi.NewInlineKeyboardButtonData("📋 Review", baseData+"review"),
		tgbotapi.NewInlineKeyboardButtonData("🚫 Ignore", baseData+"ignore"),
	)

	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(row...),
	)

	if _, err := b.api.Send(msg); err != nil {
		log.Printf("telegram send failed: %v", err)
	}
}

// askNewCourseSyncPast shows the sync-past prompt for a new course type (from worker detection).
// After selection, it advances to the next type or completes the course setup.
func (b *Bot) askNewCourseSyncPast(ctx context.Context, chatID int64, messageID int, userID int64, courseID, assignmentType, targetMode string, typeIndex, totalTypes int) {
	text := fmt.Sprintf("Would you like to sync past %s for *%s* to your Google Calendar?", typeLabel(assignmentType), courseID)
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	edit.ParseMode = "Markdown"

	// Use a special callback prefix that includes the typeIndex and totalTypes
	// so we can advance after selection: new_course_sync|COURSE_ID|TYPE|TYPE_INDEX|TOTAL_TYPES|TARGET_MODE|OPTION
	baseData := fmt.Sprintf("new_course_sync|%s|%s|%d|%d|%s|", courseID, assignmentType, typeIndex, totalTypes, targetMode)

	edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{
				tgbotapi.NewInlineKeyboardButtonData("🔮 Future only", baseData+"future"),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("📅 Past 7 days", baseData+"7d"),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("📅 Past 30 days", baseData+"30d"),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("📚 Entire course", baseData+"all"),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("🔙 Cancel", baseData+"cancel"),
			},
		},
	}
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("telegram edit failed: %v", err)
	}
}

// handleNewCourseSyncPast processes the sync-past selection for a new course type.
// After syncing past events, it advances to the next type or completes the course setup.
func (b *Bot) handleNewCourseSyncPast(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, courseID, assignmentType string, typeIndex, totalTypes int, targetMode, option string) {
	b.answerCallback(cq.ID, "")

	// If cancelled, just return to course matrix (but there's no matrix for new courses, so just acknowledge)
	if option == "cancel" {
		b.reply(cq.Message.Chat.ID, fmt.Sprintf("Cancelled. You can configure *%s* anytime from /settings.", courseID))
		return
	}

	// If "future only", no past events to sync — advance to next type
	if option == "future" {
		if typeIndex+1 < totalTypes {
			types, err := b.store.ListCourseTypes(ctx, userID, courseID)
			if err == nil && typeIndex+1 < len(types) {
				nextType := types[typeIndex+1]
				b.sendNewCoursePrompt(ctx, cq.Message.Chat.ID, userID, courseID, nextType, typeIndex+1, totalTypes)
				return
			}
		}
		b.reply(cq.Message.Chat.ID, fmt.Sprintf("✅ All set for *%s*! I'll start monitoring this course.", courseID))
		return
	}

	// Calculate cutoff and sync past events
	now := time.Now()
	var cutoff time.Time
	switch option {
	case "7d":
		cutoff = now.AddDate(0, 0, -7)
	case "30d":
		cutoff = now.AddDate(0, 0, -30)
	case "all":
		cutoff = time.Time{}
	}

	feed, err := b.store.GetFeed(ctx, userID)
	if err != nil || feed == nil {
		b.reply(cq.Message.Chat.ID, "Could not fetch Canvas feed.")
		return
	}

	events, _, err := canvas.FetchAndDetect(feed.ICalURL)
	if err != nil {
		b.reply(cq.Message.Chat.ID, "Could not fetch Canvas feed.")
		return
	}

	var pastEvents []canvas.Event
	for _, ev := range events {
		if ev.Course != courseID || ev.Type != assignmentType || ev.DueAt == nil {
			continue
		}
		if ev.DueAt.Before(now) {
			if option != "all" && ev.DueAt.Before(cutoff) {
				continue
			}
			pastEvents = append(pastEvents, ev)
		}
	}

	synced := 0
	for _, ev := range pastEvents {
		calendarID, eventID, err := b.google.CreateEventForTelegramUser(
			ctx, userID, ev.Title, *ev.DueAt,
			fmt.Sprintf("CanvasLink new-course-sync\nCourse: %s\nType: %s", ev.Course, ev.Type),
		)
		if err != nil {
			log.Printf("new course sync past event failed: %v", err)
			continue
		}
		_, err = b.store.UpsertSyncedEvent(ctx, store.SyncedEventInput{
			TelegramUserID:   userID,
			CourseID:         ev.Course,
			AssignmentType:   ev.Type,
			CanvasEventUID:   ev.UID,
			CanvasTitle:      ev.Title,
			CanvasDueAt:      *ev.DueAt,
			GoogleCalendarID: calendarID,
			GoogleEventID:    eventID,
			CanvasDTStamp:    ev.DTStamp,
			CanvasSequence:   ev.Sequence,
		})
		if err != nil {
			log.Printf("new course sync past event record failed: %v", err)
			continue
		}
		synced++
	}

	if synced > 0 {
		b.reply(cq.Message.Chat.ID, fmt.Sprintf("✅ Synced %d past %s to your Google Calendar.", synced, typeLabel(assignmentType)))
	}

	// Advance to next type or complete
	if typeIndex+1 < totalTypes {
		types, err := b.store.ListCourseTypes(ctx, userID, courseID)
		if err == nil && typeIndex+1 < len(types) {
			nextType := types[typeIndex+1]
			b.sendNewCoursePrompt(ctx, cq.Message.Chat.ID, userID, courseID, nextType, typeIndex+1, totalTypes)
			return
		}
	}

	b.reply(cq.Message.Chat.ID, fmt.Sprintf("✅ All set for *%s*! I'll start monitoring this course.", courseID))
}

// handleSyncPastOption processes the user's sync-past selection.
func (b *Bot) handleSyncPastOption(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, courseID, assignmentType, targetMode, option string) {
	b.answerCallback(cq.ID, "")

	// Save the mode first
	if err := b.store.SetCourseTypeMode(ctx, userID, courseID, assignmentType, targetMode); err != nil {
		log.Printf("set mode failed: %v", err)
		b.reply(cq.Message.Chat.ID, "Failed to save mode.")
		return
	}

	// If "future only", no past events to sync
	if option == "future" {
		b.reply(cq.Message.Chat.ID, fmt.Sprintf("✅ Mode saved. Future %s for *%s* will be synced.", typeLabel(assignmentType), courseID))
		b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
		return
	}

	// Calculate the cutoff time
	now := time.Now()
	var cutoff time.Time
	switch option {
	case "7d":
		cutoff = now.AddDate(0, 0, -7)
	case "30d":
		cutoff = now.AddDate(0, 0, -30)
	case "all":
		cutoff = time.Time{} // zero time = no cutoff
	}

	// Fetch the feed to get past events
	feed, err := b.store.GetFeed(ctx, userID)
	if err != nil || feed == nil {
		b.reply(cq.Message.Chat.ID, "Could not fetch Canvas feed.")
		b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
		return
	}

	events, _, err := canvas.FetchAndDetect(feed.ICalURL)
	if err != nil {
		b.reply(cq.Message.Chat.ID, "Could not fetch Canvas feed.")
		b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
		return
	}

	// Filter past events matching this course and type
	var pastEvents []canvas.Event
	for _, ev := range events {
		if ev.Course != courseID || ev.Type != assignmentType || ev.DueAt == nil {
			continue
		}
		if ev.DueAt.Before(now) {
			if option != "all" && ev.DueAt.Before(cutoff) {
				continue
			}
			pastEvents = append(pastEvents, ev)
		}
	}

	if len(pastEvents) == 0 {
		b.reply(cq.Message.Chat.ID, fmt.Sprintf("No past %s found for *%s* within the selected range.", typeLabel(assignmentType), courseID))
		b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
		return
	}

	// If target mode is review (Cat 2 → Review transition), show historical items prompt
	if targetMode == store.ModeReview {
		text := fmt.Sprintf("Found %d historical items.\n\nWould you like to:", len(pastEvents))
		edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, text)
		edit.ParseMode = "Markdown"
		edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{
					tgbotapi.NewInlineKeyboardButtonData("✅ Add All", CBPrefixHistorical+courseID+"|"+assignmentType+"|"+targetMode+"|add_all"),
				},
				{
					tgbotapi.NewInlineKeyboardButtonData("📋 Review Individually", CBPrefixHistorical+courseID+"|"+assignmentType+"|"+targetMode+"|review"),
				},
				{
					tgbotapi.NewInlineKeyboardButtonData("🚫 Skip Historical Items", CBPrefixHistorical+courseID+"|"+assignmentType+"|"+targetMode+"|skip"),
				},
			},
		}
		if _, err := b.api.Send(edit); err != nil {
			log.Printf("telegram edit failed: %v", err)
		}
		return
	}

	// Sync past events to Google Calendar (for Cat 1 modes)
	synced := 0
	for _, ev := range pastEvents {
		calendarID, eventID, err := b.google.CreateEventForTelegramUser(
			ctx, userID, ev.Title, *ev.DueAt,
			fmt.Sprintf("CanvasLink past-sync\nCourse: %s\nType: %s", ev.Course, ev.Type),
		)
		if err != nil {
			log.Printf("sync past event failed: %v", err)
			continue
		}
		_, err = b.store.UpsertSyncedEvent(ctx, store.SyncedEventInput{
			TelegramUserID:   userID,
			CourseID:         ev.Course,
			AssignmentType:   ev.Type,
			CanvasEventUID:   ev.UID,
			CanvasTitle:      ev.Title,
			CanvasDueAt:      *ev.DueAt,
			GoogleCalendarID: calendarID,
			GoogleEventID:    eventID,
			CanvasDTStamp:    ev.DTStamp,
			CanvasSequence:   ev.Sequence,
		})
		if err != nil {
			log.Printf("sync past event record failed: %v", err)
			continue
		}
		synced++
	}

	b.reply(cq.Message.Chat.ID, fmt.Sprintf("✅ Synced %d past %s to your Google Calendar.", synced, typeLabel(assignmentType)))
	b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
}

// handleRemoveCalOption processes the user's response to remove-from-calendar prompt.
func (b *Bot) handleRemoveCalOption(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, courseID, assignmentType, targetMode, action string) {
	b.answerCallback(cq.ID, "")

	if action == "cancel" {
		b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
		return
	}

	// Save the mode
	if err := b.store.SetCourseTypeMode(ctx, userID, courseID, assignmentType, targetMode); err != nil {
		log.Printf("set mode failed: %v", err)
		b.reply(cq.Message.Chat.ID, "Failed to save mode.")
		return
	}

	if action == "yes" {
		// Remove all synced events for this course from Google Calendar
		synced, err := b.store.ListSyncedEventsByCourse(ctx, userID, courseID)
		if err == nil {
			removed := 0
			for _, se := range synced {
				if se.GoogleEventID != "" && b.google != nil {
					if err := b.google.DeleteEvent(ctx, userID, se.GoogleCalendarID, se.GoogleEventID); err != nil {
						log.Printf("delete google event failed: %v", err)
					}
				}
				removed++
			}
			// Delete synced event records for this course
			_ = b.store.DeleteSyncedEventsByCourse(ctx, userID, courseID)
			b.reply(cq.Message.Chat.ID, fmt.Sprintf("✅ Removed %d events from your Google Calendar.", removed))
		}
	}

	b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
}

// handleHistoricalOption processes the user's response to the historical items prompt.
func (b *Bot) handleHistoricalOption(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, courseID, assignmentType, targetMode, option string) {
	b.answerCallback(cq.ID, "")

	// Save the mode
	if err := b.store.SetCourseTypeMode(ctx, userID, courseID, assignmentType, targetMode); err != nil {
		log.Printf("set mode failed: %v", err)
		b.reply(cq.Message.Chat.ID, "Failed to save mode.")
		return
	}

	switch option {
	case "skip":
		b.reply(cq.Message.Chat.ID, fmt.Sprintf("✅ Mode saved. Historical %s will be skipped.", typeLabel(assignmentType)))
	case "add_all":
		// Fetch feed and sync all past events for this course/type
		feed, err := b.store.GetFeed(ctx, userID)
		if err != nil || feed == nil {
			b.reply(cq.Message.Chat.ID, "Could not fetch Canvas feed.")
			b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
			return
		}
		events, _, err := canvas.FetchAndDetect(feed.ICalURL)
		if err != nil {
			b.reply(cq.Message.Chat.ID, "Could not fetch Canvas feed.")
			b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
			return
		}
		now := time.Now()
		synced := 0
		for _, ev := range events {
			if ev.Course != courseID || ev.Type != assignmentType || ev.DueAt == nil {
				continue
			}
			if ev.DueAt.Before(now) {
				calendarID, eventID, err := b.google.CreateEventForTelegramUser(
					ctx, userID, ev.Title, *ev.DueAt,
					fmt.Sprintf("CanvasLink historical-sync\nCourse: %s\nType: %s", ev.Course, ev.Type),
				)
				if err != nil {
					continue
				}
				_, err = b.store.UpsertSyncedEvent(ctx, store.SyncedEventInput{
					TelegramUserID:   userID,
					CourseID:         ev.Course,
					AssignmentType:   ev.Type,
					CanvasEventUID:   ev.UID,
					CanvasTitle:      ev.Title,
					CanvasDueAt:      *ev.DueAt,
					GoogleCalendarID: calendarID,
					GoogleEventID:    eventID,
					CanvasDTStamp:    ev.DTStamp,
					CanvasSequence:   ev.Sequence,
				})
				if err != nil {
					continue
				}
				synced++
			}
		}
		b.reply(cq.Message.Chat.ID, fmt.Sprintf("✅ Added %d historical %s to your Google Calendar.", synced, typeLabel(assignmentType)))
	case "review":
		// Create pending actions for each historical event so user can review individually
		feed, err := b.store.GetFeed(ctx, userID)
		if err != nil || feed == nil {
			b.reply(cq.Message.Chat.ID, "Could not fetch Canvas feed.")
			b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
			return
		}
		events, _, err := canvas.FetchAndDetect(feed.ICalURL)
		if err != nil {
			b.reply(cq.Message.Chat.ID, "Could not fetch Canvas feed.")
			b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
			return
		}
		account, _ := b.store.GetTelegramAccount(ctx, userID)
		now := time.Now()
		created := 0
		for _, ev := range events {
			if ev.Course != courseID || ev.Type != assignmentType || ev.DueAt == nil {
				continue
			}
			if ev.DueAt.Before(now) {
				if account != nil {
					_, inserted, _ := b.store.CreatePendingActionIfAbsent(ctx, store.PendingActionInput{
						TelegramUserID: userID,
						CourseID:       ev.Course,
						AssignmentType: ev.Type,
						CanvasEventUID: ev.UID,
						CanvasTitle:    ev.Title,
						CanvasDueAt:    *ev.DueAt,
						TelegramChatID: account.ChatID,
					})
					if inserted {
						created++
					}
				}
			}
		}
		b.reply(cq.Message.Chat.ID, fmt.Sprintf("📋 Created %d review requests for historical %s.", created, typeLabel(assignmentType)))
	}

	b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
}

func typeLabel(t string) string {
	switch t {
	case "assignment":
		return "📝 Assignment"
	case "quiz":
		return "📋 Quiz"
	case "discussion":
		return "💬 Discussion"
	case "exam":
		return "📄 Exam"
	case "project":
		return "📁 Project"
	case "test":
		return "📄 Test"
	case "midterm":
		return "📄 Midterm"
	case "final":
		return "📄 Final"
	case "presentation":
		return "📊 Presentation"
	case "essay":
		return "📝 Essay"
	case "homework":
		return "📚 Homework"
	case "lab":
		return "🔬 Lab"
	case "reading":
		return "📖 Reading"
	case "other":
		return "📌 Other"
	default:
		return t
	}
}

func uniqueCourseCount(events []canvas.Event) int {
	seen := map[string]bool{}
	for _, e := range events {
		if e.Course != "" {
			seen[e.Course] = true
		}
	}
	return len(seen)
}

// BuildSettingsMatrixMessage is the exported version for use by the sync worker.
func BuildSettingsMatrixMessage(courseID string, settings []store.CourseSetting) string {
	msg := buildSettingsMatrixMessage(courseID, settings)
	return msg.Text
}

func (b *Bot) registerCommands() error {
	cmds := []tgbotapi.BotCommand{
		{Command: "start", Description: "Begin or restart onboarding"},
		{Command: "help", Description: "Show the full user guide"},
		{Command: "settings", Description: "Open settings dashboard"},
	}
	cfg := tgbotapi.NewSetMyCommands(cmds...)
	_, err := b.api.Request(cfg)
	return err
}