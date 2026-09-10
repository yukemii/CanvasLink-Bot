package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/markadodo/canvaslink/internal/canvas"
	canvasGoogle "github.com/markadodo/canvaslink/internal/google"
	"github.com/markadodo/canvaslink/internal/oauth"
	"github.com/markadodo/canvaslink/internal/store"
	telegramHTTP "github.com/markadodo/canvaslink/internal/telegram"
)

// Onboarding status constants
const (
	OnboardingAwaitingURL  = "awaiting_url"
	OnboardingGooglePrompt = "google_prompt"
	OnboardingCourseSetup  = "course_setup"

	destructiveConfirmationTTL = 10 * time.Minute
)

// Callback data prefixes
const (
	CBPrefixOnboardGoogleYes = "ob_google_yes"
	CBPrefixOnboardGoogleNo  = "ob_google_no"
	CBPrefixOnboardMode      = "ob_mode|"
	CBPrefixSettingsHub      = "settings|"
	CBPrefixCourse           = "course|"
	CBPrefixModules          = "modules"
	CBPrefixMode             = "mode|"
	CBPrefixPending          = "pending|"
	CBPrefixNoop             = "noop|"
	CBPrefixConfirm          = "confirm|"
	CBPrefixCancel           = "cancel"
)

var errWipeNeedsGoogle = errors.New("google calendar must be connected to wipe tracked events")

type Bot struct {
	api         *tgbotapi.BotAPI
	store       *store.Store
	oauthServer *oauth.Server
	google      *canvasGoogle.CalendarClient

	defaultTimezone string
	oauthNotifyCh   chan int64
}

func New(token string, db *store.Store, oauthServer *oauth.Server, googleClient *canvasGoogle.CalendarClient, defaultTimezone string) (*Bot, error) {
	if db == nil {
		return nil, errors.New("bot store is required")
	}
	if oauthServer == nil {
		return nil, errors.New("oauth server is required")
	}
	if googleClient == nil {
		return nil, errors.New("google calendar client is required")
	}
	if err := store.ValidateTimezone(defaultTimezone); err != nil {
		return nil, fmt.Errorf("default timezone: %w", err)
	}
	api, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, telegramHTTP.NewClient(token, 70*time.Second))
	if err != nil {
		return nil, err
	}
	return &Bot{
		api:             api,
		store:           db,
		oauthServer:     oauthServer,
		google:          googleClient,
		defaultTimezone: defaultTimezone,
		oauthNotifyCh:   make(chan int64, 100),
	}, nil
}

func (b *Bot) Start(ctx context.Context) error {
	log.Printf("CanvasLink bot is online as @%s", b.api.Self.UserName)
	if err := b.registerCommands(); err != nil {
		log.Printf("register telegram commands failed: %v", err)
	}

	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 60
	updates := b.api.GetUpdatesChan(updateConfig)
	defer b.api.StopReceivingUpdates()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case update, ok := <-updates:
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return errors.New("telegram updates channel closed")
			}
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
	select {
	case b.oauthNotifyCh <- telegramUserID:
	default:
		log.Printf("oauth completion queue full user=%d", telegramUserID)
	}
}

func (b *Bot) handleOAuthComplete(ctx context.Context, userID int64) {
	account, err := oauthCompletionAccount(
		ctx,
		userID,
		b.store.GetTelegramAccount,
		b.oauthServer.IsConnected,
	)
	if err != nil {
		log.Printf("validate account after oauth failed user=%d err=%v", userID, err)
		return
	}
	if account == nil {
		return
	}

	b.reply(account.ChatID, "✅ Google Calendar connected! Now let's set up your sync preferences.")
	b.sendModesGuide(ctx, account.ChatID, userID)
}

func oauthCompletionAccount(
	ctx context.Context,
	userID int64,
	loadAccount func(context.Context, int64) (*store.TelegramAccount, error),
	isConnected func(context.Context, int64) (bool, error),
) (*store.TelegramAccount, error) {
	account, err := loadAccount(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load Telegram account: %w", err)
	}
	if account == nil || account.OnboardingStatus != OnboardingGooglePrompt {
		return nil, nil
	}

	connected, err := isConnected(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("check current Google connection: %w", err)
	}
	if !connected {
		return nil, nil
	}
	return account, nil
}

// ---------------------------------------------------------------------------
// Message handler
// ---------------------------------------------------------------------------

func (b *Bot) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	if msg.Chat == nil {
		return
	}
	if msg.Chat.Type != "private" {
		if msg.IsCommand() {
			b.reply(msg.Chat.ID, "For privacy and account safety, please use CanvasLink in a private chat with the bot.")
		}
		return
	}

	userID := int64(msg.From.ID)
	if msg.Chat.ID != userID {
		log.Printf("refusing Telegram message with mismatched private binding user=%d", userID)
		return
	}
	text := strings.TrimSpace(msg.Text)

	if msg.IsCommand() {
		b.handleCommand(ctx, msg, userID, msg.Command())
		return
	}

	// Non-command text — check if user is in onboarding
	account, err := b.store.GetTelegramAccount(ctx, userID)
	if err != nil {
		log.Printf("get account failed: %v", err)
		b.reply(msg.Chat.ID, "Something went wrong. Please try again.")
		return
	}

	if account != nil && account.OnboardingStatus == OnboardingAwaitingURL {
		// Treat as potential iCal URL
		b.handleOnboardingURL(ctx, msg, userID, account.StateRevision, text)
		return
	}

	if b.handlePlannerText(ctx, msg, userID, text) {
		return
	}

	// Default fallback
	b.reply(msg.Chat.ID, "Try /today or /upcoming to see your work, /settings to customize, or /help for commands. Send /start if you haven’t set up yet.")
}

func (b *Bot) handleCommand(ctx context.Context, msg *tgbotapi.Message, userID int64, command string) {
	if b.handlePlannerCommand(ctx, msg, userID, command) {
		return
	}
	switch command {
	case "help":
		b.reply(msg.Chat.ID, helpMessage())
	case "start":
		b.handleStart(ctx, msg, userID)
	case "connect_google":
		b.handleConnectGoogle(ctx, msg)
	case "settings":
		b.handleSettings(ctx, msg, userID)
	case "timezone":
		b.handleTimezone(ctx, msg, userID)
	case "disconnect_canvas":
		b.handleDisconnectCanvas(ctx, msg, userID)
	case "disconnect_google":
		b.handleDisconnectGoogle(ctx, msg, userID)
	case "reset":
		b.handleReset(ctx, msg, userID)
	default:
		b.reply(msg.Chat.ID, "Unknown command. Use /help for the full command list, or /start to begin setup.")
	}
}

// ---------------------------------------------------------------------------
// /start — Onboarding wizard
// ---------------------------------------------------------------------------

func (b *Bot) handleStart(ctx context.Context, msg *tgbotapi.Message, userID int64) {
	// Ensure telegram account exists
	if err := b.store.UpsertTelegramAccountWithTimezone(ctx, userID, msg.Chat.ID, msg.From.UserName, b.defaultTimezone); err != nil {
		log.Printf("upsert telegram account failed: %v", err)
		b.reply(msg.Chat.ID, "Something went wrong. Please try again.")
		return
	}

	// Check if user already has a feed
	hasFeed, err := b.store.HasFeed(ctx, userID)
	if err != nil {
		log.Printf("has feed check failed: %v", err)
		b.reply(msg.Chat.ID, "Something went wrong. Please try again.")
		return
	}

	if hasFeed {
		account, accountErr := b.store.GetTelegramAccount(ctx, userID)
		if accountErr != nil {
			log.Printf("get onboarding status failed: %v", accountErr)
		}
		if account != nil {
			switch account.OnboardingStatus {
			case OnboardingGooglePrompt:
				if !b.oauthServer.IsConfigured() {
					b.sendModesGuide(ctx, msg.Chat.ID, userID)
					return
				}
				connected, err := b.oauthServer.IsConnected(ctx, userID)
				if err != nil {
					log.Printf("google status check failed: %v", err)
					b.reply(msg.Chat.ID, "I could not check Google status right now. Please try again.")
					return
				}
				if connected {
					b.sendModesGuide(ctx, msg.Chat.ID, userID)
				} else {
					b.sendGooglePrompt(msg.Chat.ID, 0, 0)
				}
				return
			case OnboardingCourseSetup:
				b.reply(msg.Chat.ID, "Let's resume your module preferences.")
				b.sendModesGuide(ctx, msg.Chat.ID, userID)
				return
			}
		}
		b.reply(msg.Chat.ID, returningMessage)
		return
	}

	// Start onboarding
	if err := b.store.SetOnboardingStatus(ctx, userID, OnboardingAwaitingURL); err != nil {
		log.Printf("set onboarding status failed: %v", err)
		b.reply(msg.Chat.ID, "Something went wrong. Please try again.")
		return
	}

	b.reply(msg.Chat.ID, welcomeMessage)
}

// ---------------------------------------------------------------------------
// Onboarding: URL paste handler
// ---------------------------------------------------------------------------

func (b *Bot) handleOnboardingURL(
	ctx context.Context,
	msg *tgbotapi.Message,
	userID,
	expectedStateRevision int64,
	text string,
) {
	// Validate it looks like a URL
	parsedURL, err := url.ParseRequestURI(text)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		b.reply(msg.Chat.ID, "That doesn't look like a valid URL. Please paste the iCal feed URL from Canvas (it should start with https:// and end in .ics).")
		return
	}
	deleteMessage := tgbotapi.NewDeleteMessage(msg.Chat.ID, msg.MessageID)
	if _, err := b.api.Request(deleteMessage); err != nil {
		log.Printf("delete sensitive Canvas feed message failed user=%d: %v", userID, err)
		b.reply(msg.Chat.ID, "For privacy, please delete the message containing your Canvas feed link from this chat after setup.")
	}

	// Validate the feed before persisting it so a bad URL cannot leave onboarding
	// stuck in a falsely connected state.
	events, seeds, err := canvas.FetchAndDetectContext(ctx, text)
	if err != nil {
		log.Printf("canvas parse failed: %v", err)
		b.reply(msg.Chat.ID, "I could not read that as a Canvas calendar feed. Please check the link and paste it again.")
		return
	}
	if len(seeds) == 0 {
		b.reply(msg.Chat.ID, "✅ Your calendar feed is valid but has no recognizable modules yet. I’ll keep checking as your semester gets published.")
	}

	// The network fetch above can outlive a concurrent disconnect/reset on
	// another instance. Serialize only the commit and require the exact account
	// revision/status observed before the fetch so stale work cannot reconnect.
	release, acquired, err := b.store.TryFeedSyncLock(ctx, userID)
	if err != nil {
		log.Printf("lock onboarding feed save failed user=%d: %v", userID, err)
		b.reply(msg.Chat.ID, "I validated your feed but could not save it safely. Please paste the link again.")
		return
	}
	if !acquired {
		b.reply(msg.Chat.ID, "Another account change is finishing. Please paste the Canvas link again in a moment.")
		return
	}
	saveErr := b.store.SaveFeedWithSettings(
		ctx,
		userID,
		text,
		OnboardingAwaitingURL,
		OnboardingGooglePrompt,
		expectedStateRevision,
		seeds,
	)
	if err := release(); err != nil {
		log.Printf("release onboarding feed-save lock failed user=%d: %v", userID, err)
	}
	if saveErr != nil {
		log.Printf("save feed and settings failed: %v", saveErr)
		if errors.Is(saveErr, store.ErrStaleOnboardingStep) {
			b.reply(msg.Chat.ID, "Your account changed while I checked the feed, so I did not reconnect it. Send /start and paste the link again.")
		} else {
			b.reply(msg.Chat.ID, "I parsed your feed, but could not save it. Please try again.")
		}
		return
	}

	courseCount := uniqueCourseCount(events)
	eventCount := upcomingEventCount(events)
	zone, _ := b.store.GetUserTimezone(ctx, userID)
	b.reply(msg.Chat.ID, onboardingPreview(events, zone, time.Now()))

	if !b.oauthServer.IsConfigured() {
		b.reply(msg.Chat.ID, fmt.Sprintf("✅ Canvas connected! I found %d modules with %d upcoming events.\n\nGoogle Calendar is not configured on this bot, so we'll continue with Telegram notifications.", courseCount, eventCount))
		b.sendModesGuide(ctx, msg.Chat.ID, userID)
		return
	}

	b.sendGooglePrompt(msg.Chat.ID, courseCount, eventCount)
}

func (b *Bot) sendGooglePrompt(chatID int64, courseCount, eventCount int) {
	msgText := "Would you like to connect Google Calendar?\n\nYour assignments can automatically appear in your calendar. If you skip this, you can still receive notifications here in Telegram."
	if courseCount > 0 {
		msgText = fmt.Sprintf("✅ Canvas connected! I found %d modules with %d upcoming events.\n\n%s", courseCount, eventCount, msgText)
	}
	replyMsg := tgbotapi.NewMessage(chatID, msgText)
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
	if !b.hasOnboardingStatus(ctx, userID, OnboardingGooglePrompt) {
		b.answerCallback(cq.ID, "This setup step has expired")
		return
	}
	b.answerCallback(cq.ID, "")
	b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)

	// Check if already connected
	connected, err := b.oauthServer.IsConnected(ctx, userID)
	if err != nil {
		log.Printf("google status check failed: %v", err)
		b.reply(cq.Message.Chat.ID, "I could not check Google status right now.")
		return
	}
	if connected {
		b.reply(cq.Message.Chat.ID, "Your Google Calendar is already connected!")
		b.sendModesGuide(ctx, cq.Message.Chat.ID, userID)
		return
	}

	// Generate auth URL
	authURL, err := b.oauthServer.AuthURL(ctx, userID)
	if err != nil {
		log.Printf("google auth url generation failed: %v", err)
		b.reply(cq.Message.Chat.ID, "I could not get a Google connect link right now.")
		return
	}

	b.sendGoogleAuthButton(
		cq.Message.Chat.ID,
		authURL,
		"Connect Google Calendar, then return here and I'll continue setting up your modules.",
	)
}

func (b *Bot) handleOnboardingGoogleNo(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64) {
	if !b.hasOnboardingStatus(ctx, userID, OnboardingGooglePrompt) {
		b.answerCallback(cq.ID, "This setup step has expired")
		return
	}
	b.answerCallback(cq.ID, "")
	b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
	b.reply(cq.Message.Chat.ID, "No problem! You can still receive notifications here in Telegram.\nYou can connect Google Calendar anytime with /connect_google.\n\nSince Google Calendar isn't connected, all modules will be set to 🔔 Active mode (you'll get notifications here). You can change this later with /settings.")

	// Seed default settings as Active (already the default in SeedDefaultSettings)
	// No Auto mode available until Google is connected
	b.sendModesGuide(ctx, cq.Message.Chat.ID, userID)
}

// ---------------------------------------------------------------------------
// Onboarding: One-by-one course setup
// ---------------------------------------------------------------------------

// sendModesGuide explains the three sync modes and then starts the course setup prompts.
func (b *Bot) sendModesGuide(ctx context.Context, chatID int64, userID int64) {
	googleConnected, err := b.oauthServer.IsConnected(ctx, userID)
	if err != nil {
		log.Printf("google status check failed: %v", err)
	}

	var guide string
	if googleConnected {
		guide = `📚 Sync Modes Guide

Each module has event types (assignments, quizzes, etc.). You can set how each type behaves:

🚀 Auto — Events are automatically added to your Google Calendar without asking each time.
🔔 Active — You'll receive a notification here in Telegram asking if you want to add it to your calendar.
🚫 Ignore — Events are filtered out and ignored.

Let's set up your preferences now!`
	} else {
		guide = `📚 Sync Modes Guide

Each module has event types (assignments, quizzes, etc.). You can set how each type behaves:

🔔 Active — You'll receive a notification here in Telegram asking if you want to add it to your calendar.
🚫 Ignore — Events are filtered out and ignored.

🚀 Auto mode is only available after connecting Google Calendar with /connect_google.

Let's set up your preferences now!`
	}

	b.reply(chatID, guide)
	b.sendQuickSetup(ctx, chatID, userID, googleConnected)
}

func (b *Bot) startCourseSetup(ctx context.Context, chatID int64, userID int64) {
	account, err := b.store.GetTelegramAccount(ctx, userID)
	if err != nil {
		log.Printf("load onboarding account failed: %v", err)
		b.reply(chatID, "I could not load your modules. Please try /start again.")
		return
	}
	if account == nil {
		b.reply(chatID, "I could not find your setup. Please send /start again.")
		return
	}

	if account.OnboardingStatus != OnboardingCourseSetup || !account.OnboardingSettingID.Valid {
		first, err := b.store.BeginOnboardingCourseSetup(ctx, userID)
		if err != nil {
			log.Printf("begin course setup failed: %v", err)
			b.reply(chatID, "I could not save onboarding progress. Please try /start again.")
			return
		}
		if first == nil {
			b.finishOnboarding(ctx, chatID, userID)
			return
		}
	}

	b.sendCourseSetupPrompt(ctx, chatID, userID)
}

func (b *Bot) sendCourseSetupPrompt(ctx context.Context, chatID int64, userID int64) {
	account, err := b.store.GetTelegramAccount(ctx, userID)
	if err != nil || account == nil {
		log.Printf("load onboarding progress failed: %v", err)
		b.reply(chatID, "I could not load your onboarding progress. Please try /start again.")
		return
	}
	if account.OnboardingStatus != OnboardingCourseSetup || !account.OnboardingSettingID.Valid {
		b.finishOnboarding(ctx, chatID, userID)
		return
	}

	settings, err := b.store.ListAllCourseSettings(ctx, userID)
	if err != nil {
		log.Printf("list onboarding settings failed: %v", err)
		b.reply(chatID, "I could not load your module settings. Please try /start again.")
		return
	}
	currentIndex := -1
	for i := range settings {
		if settings[i].ID == account.OnboardingSettingID.Int64 {
			currentIndex = i
			break
		}
	}
	if currentIndex < 0 {
		log.Printf("onboarding setting missing user=%d setting=%d", userID, account.OnboardingSettingID.Int64)
		b.reply(chatID, "Your module list changed while setting up. Send /start to safely restart this section.")
		return
	}
	setting := settings[currentIndex]

	msg := fmt.Sprintf("📚 Step %d/%d: %s\nHow should I handle %s for this module?",
		currentIndex+1, len(settings), setting.CourseID, typeLabel(setting.AssignmentType))

	// Only show Auto button if Google Calendar is connected
	googleConnected, err := b.oauthServer.IsConnected(ctx, userID)
	if err != nil {
		log.Printf("google status check failed: %v", err)
	}

	var row []tgbotapi.InlineKeyboardButton
	if googleConnected {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("🚀 Auto", fmt.Sprintf("%s%d|%s", CBPrefixOnboardMode, setting.ID, store.ModeAuto)))
	}
	row = append(row,
		tgbotapi.NewInlineKeyboardButtonData("🔔 Active", fmt.Sprintf("%s%d|%s", CBPrefixOnboardMode, setting.ID, store.ModeActive)),
		tgbotapi.NewInlineKeyboardButtonData("🚫 Ignore", fmt.Sprintf("%s%d|%s", CBPrefixOnboardMode, setting.ID, store.ModeIgnore)),
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
	// New format: ob_mode|SETTING_ID|MODE. Four-part callbacks remain
	// supported for messages sent before short IDs were introduced.
	parts := strings.Split(data, "|")
	if len(parts) != 3 && len(parts) != 4 {
		b.answerCallback(cq.ID, "Invalid")
		return
	}
	var (
		settingID int64
		mode      string
	)
	if len(parts) == 3 {
		var err error
		settingID, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || settingID <= 0 {
			b.answerCallback(cq.ID, "Invalid")
			return
		}
		mode = parts[2]
	} else {
		mode = parts[3]
		settings, err := b.store.ListCourseSettings(ctx, userID, parts[1])
		if err != nil {
			log.Printf("resolve legacy onboarding setting failed: %v", err)
			b.answerCallback(cq.ID, "This setup step is no longer valid")
			return
		}
		for _, setting := range settings {
			if setting.AssignmentType == parts[2] {
				settingID = setting.ID
				break
			}
		}
		if settingID == 0 {
			b.answerCallback(cq.ID, "This setup step is no longer valid")
			return
		}
	}

	if mode != store.ModeAuto && mode != store.ModeActive && mode != store.ModeIgnore {
		b.answerCallback(cq.ID, "Invalid")
		return
	}
	release, acquired, err := b.store.TryFeedSyncLock(ctx, userID)
	if err != nil {
		log.Printf("lock onboarding mode failed user=%d err=%v", userID, err)
		b.answerCallback(cq.ID, "Could not save this safely")
		return
	}
	if !acquired {
		b.answerCallback(cq.ID, "A sync is finishing; try again in a moment")
		return
	}
	defer func() {
		if err := release(); err != nil {
			log.Printf("release onboarding-mode lock failed user=%d err=%v", userID, err)
		}
	}()
	if !b.hasOnboardingStatus(ctx, userID, OnboardingCourseSetup) {
		b.answerCallback(cq.ID, "This setup step has expired")
		return
	}
	if mode == store.ModeAuto {
		connected, err := b.oauthServer.IsConnected(ctx, userID)
		if err != nil || !connected {
			b.answerCallback(cq.ID, "Connect Google before using Auto")
			return
		}
	}

	_, completed, err := b.store.SetModeAndAdvanceOnboardingCourseSetup(ctx, userID, settingID, mode)
	if err != nil {
		log.Printf("save and advance onboarding mode failed: %v", err)
		b.answerCallback(cq.ID, "That setup step is no longer current")
		return
	}

	b.answerCallback(cq.ID, "Saved ✅")
	b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)

	if completed {
		b.sendOnboardingComplete(ctx, cq.Message.Chat.ID, userID)
		return
	}
	b.sendCourseSetupPrompt(ctx, cq.Message.Chat.ID, userID)
}

func (b *Bot) finishOnboarding(ctx context.Context, chatID int64, userID int64) {
	account, err := b.store.GetTelegramAccount(ctx, userID)
	if err != nil {
		log.Printf("load onboarding completion state failed: %v", err)
		b.reply(chatID, "I could not finish saving your setup. Please try /start again.")
		return
	}
	if account != nil && account.OnboardingSettingID.Valid {
		err = b.store.ClearOnboardingCourseSetup(ctx, userID, account.OnboardingSettingID.Int64)
	} else {
		err = b.store.SetOnboardingStatus(ctx, userID, "")
	}
	if err != nil {
		log.Printf("clear onboarding status failed: %v", err)
		b.reply(chatID, "I could not finish saving your setup. Please try /start again.")
		return
	}
	b.sendOnboardingComplete(ctx, chatID, userID)
}

func (b *Bot) sendOnboardingComplete(ctx context.Context, chatID int64, userID int64) {
	prefs, err := b.store.PlannerPreferences(ctx, userID)
	if err != nil {
		prefs = store.DefaultPlannerPreferences()
	}
	reminderSummary := plannerReminderSummary(prefs)
	// Get summary
	courses, err := b.store.ListUserCourses(ctx, userID)
	courseCount := len(courses)
	if err != nil {
		courseCount = 0
	}

	b.reply(chatID, fmt.Sprintf(`✅ All set! CanvasLink is now monitoring your Canvas feed.

Here's a summary:
📚 %d modules configured
🔔 Active mode: you'll get a notification for each new event
🚀 Auto mode: events go straight to your calendar
🚫 Ignore mode: events are filtered out

You can toggle any course or assignment type anytime in /settings.

⏰ %s
Customize or disable reminders in Settings → Reminders. For all-day items, offsets count back from 09:00 on the due date.

📋 Use /today, /week or /upcoming. Scheduled daily/weekly agendas are optional in Settings → Agendas.

Google events go into a separate CanvasLink calendar by default; choose another in Settings → Calendar.

I’ll send a first-check summary when your feed is processed.

I'll keep checking for new events automatically. Use /help for commands and defaults.`, courseCount, reminderSummary))
}

// ---------------------------------------------------------------------------
// /connect_google
// ---------------------------------------------------------------------------

func (b *Bot) handleConnectGoogle(ctx context.Context, msg *tgbotapi.Message) {
	if msg == nil || msg.From == nil {
		return
	}
	telegramUserID := int64(msg.From.ID)
	if err := b.store.UpsertTelegramAccountWithTimezone(
		ctx,
		telegramUserID,
		msg.Chat.ID,
		msg.From.UserName,
		b.defaultTimezone,
	); err != nil {
		log.Printf("ensure private account before Google authorization failed user=%d: %v", telegramUserID, err)
		b.reply(msg.Chat.ID, "I could not prepare your private account for Google Calendar. Please try /start first.")
		return
	}
	if !b.oauthServer.IsConfigured() {
		b.reply(msg.Chat.ID, "Google Calendar is not configured on this bot.")
		return
	}

	// Check if already connected
	connected, err := b.oauthServer.IsConnected(ctx, telegramUserID)
	if err != nil {
		log.Printf("google status check failed: %v", err)
		b.reply(msg.Chat.ID, "I could not check Google status right now.")
		return
	}
	if connected {
		b.reply(msg.Chat.ID, "Your Google Calendar is already connected.")
		return
	}

	// Generate auth URL
	authURL, err := b.oauthServer.AuthURL(ctx, telegramUserID)
	if err != nil {
		log.Printf("google auth url generation failed: %v", err)
		b.reply(msg.Chat.ID, "I could not get a Google connect link right now.")
		return
	}
	b.sendGoogleAuthButton(msg.Chat.ID, authURL, "Use the button below to securely connect Google Calendar.")
}

// ---------------------------------------------------------------------------
// /settings — Hub menu
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

	replyMsg := settingsMessage(msg.Chat.ID)
	if _, err := b.api.Send(replyMsg); err != nil {
		log.Printf("telegram send failed: %v", err)
	}
}

func (b *Bot) handleSettingsCallback(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, action string) {
	b.answerCallback(cq.ID, "")

	switch action {
	case "main":
		edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, "⚙️ CanvasLink Settings\n\nChoose an option:")
		edit.ReplyMarkup = settingsKeyboard()
		if _, err := b.api.Send(edit); err != nil {
			log.Printf("telegram edit failed: %v", err)
		}
	case "modules":
		b.editModulesMenu(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID)
	case "google":
		connected, err := b.oauthServer.IsConnected(ctx, userID)
		if err != nil {
			log.Printf("google status check failed: %v", err)
			b.reply(cq.Message.Chat.ID, "Could not check Google status.")
			return
		}
		if connected {
			b.reply(cq.Message.Chat.ID, "✅ Google Calendar is connected.\n\nUse /disconnect_google to remove the connection.")
		} else {
			b.reply(cq.Message.Chat.ID, "❌ Google Calendar is not connected.\n\nUse /connect_google to link your calendar.")
		}
	case "feed":
		b.reply(cq.Message.Chat.ID, "📡 Your Canvas feed is connected.\n\nUse /disconnect_canvas to remove it and start over.")
	case "timezone":
		timezone, err := b.store.GetUserTimezone(ctx, userID)
		if err != nil {
			log.Printf("get timezone failed: %v", err)
			b.reply(cq.Message.Chat.ID, "I could not load your timezone.")
			return
		}
		b.reply(cq.Message.Chat.ID, fmt.Sprintf("🕒 Your timezone is %s.\n\nChange it with /timezone Region/City — for example, /timezone Asia/Singapore.", timezone))
	case "reset":
		b.sendResetConfirmation(ctx, cq.Message.Chat.ID, userID)
	default:
		b.reply(cq.Message.Chat.ID, "Unknown option.")
	}
}

func (b *Bot) handleTimezone(ctx context.Context, msg *tgbotapi.Message, userID int64) {
	if err := b.store.UpsertTelegramAccountWithTimezone(ctx, userID, msg.Chat.ID, msg.From.UserName, b.defaultTimezone); err != nil {
		log.Printf("ensure account for timezone failed: %v", err)
		b.reply(msg.Chat.ID, "I could not save your timezone right now.")
		return
	}

	timezone := strings.TrimSpace(msg.CommandArguments())
	if timezone == "" {
		current, err := b.store.GetUserTimezone(ctx, userID)
		if err != nil {
			log.Printf("get timezone failed: %v", err)
			b.reply(msg.Chat.ID, "I could not load your timezone right now.")
			return
		}
		b.reply(msg.Chat.ID, fmt.Sprintf("Your timezone is %s.\n\nChange it with /timezone Region/City — for example, /timezone Asia/Singapore.", current))
		return
	}
	if err := store.ValidateTimezone(timezone); err != nil {
		b.reply(msg.Chat.ID, "That is not a valid IANA timezone. Try a name such as Asia/Singapore, Europe/London, or America/New_York.")
		return
	}
	if err := b.store.SetUserTimezone(ctx, userID, timezone); err != nil {
		log.Printf("set timezone failed: %v", err)
		b.reply(msg.Chat.ID, "I could not save your timezone right now.")
		return
	}
	b.reply(msg.Chat.ID, fmt.Sprintf("✅ Timezone updated to %s.", timezone))
}

// ---------------------------------------------------------------------------
// /disconnect_canvas
// ---------------------------------------------------------------------------

func (b *Bot) handleDisconnectCanvas(ctx context.Context, msg *tgbotapi.Message, userID int64) {
	hasFeed, err := b.store.HasFeed(ctx, userID)
	if err != nil {
		log.Printf("has feed check failed: %v", err)
		b.reply(msg.Chat.ID, "I could not check your Canvas connection. Please try again.")
		return
	}
	if !hasFeed {
		b.reply(msg.Chat.ID, "You don't have a Canvas feed connected.")
		return
	}
	nonce, err := b.store.CreateDestructiveConfirmation(ctx, userID, "disconnect_canvas", destructiveConfirmationTTL)
	if err != nil {
		log.Printf("create disconnect Canvas confirmation failed: %v", err)
		b.reply(msg.Chat.ID, "I could not prepare a secure confirmation. Please try again.")
		return
	}

	confirmMsg := tgbotapi.NewMessage(msg.Chat.ID, `⚠️ Disconnect Canvas?

This will:
• Remove your Canvas feed URL
• Delete all your sync settings (course modes)
• Events already in Google Calendar will NOT be removed

You'll need to go through onboarding again to reconnect.

Are you sure?`)
	confirmMsg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Yes, disconnect", CBPrefixConfirm+"disconnect_canvas|"+nonce),
			tgbotapi.NewInlineKeyboardButtonData("🔙 Cancel", CBPrefixCancel),
		),
	)
	if _, err := b.api.Send(confirmMsg); err != nil {
		log.Printf("telegram send failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// /disconnect_google
// ---------------------------------------------------------------------------

func (b *Bot) handleDisconnectGoogle(ctx context.Context, msg *tgbotapi.Message, userID int64) {
	connected, err := b.store.HasGoogleToken(ctx, userID)
	if err != nil {
		log.Printf("google status check failed: %v", err)
		b.reply(msg.Chat.ID, "I could not check your Google connection. Please try again.")
		return
	}
	if !connected {
		b.reply(msg.Chat.ID, "Google Calendar is not connected.")
		return
	}
	nonce, err := b.store.CreateDestructiveConfirmation(ctx, userID, "disconnect_google", destructiveConfirmationTTL)
	if err != nil {
		log.Printf("create disconnect Google confirmation failed: %v", err)
		b.reply(msg.Chat.ID, "I could not prepare a secure confirmation. Please try again.")
		return
	}

	confirmMsg := tgbotapi.NewMessage(msg.Chat.ID, `⚠️ Disconnect Google Calendar?

This will:
• Remove your Google Calendar connection
• Events already in Google Calendar will NOT be removed
• New events will still be detected but won't sync to Google

Are you sure?`)
	confirmMsg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Yes, disconnect", CBPrefixConfirm+"disconnect_google|"+nonce),
			tgbotapi.NewInlineKeyboardButtonData("🔙 Cancel", CBPrefixCancel),
		),
	)
	if _, err := b.api.Send(confirmMsg); err != nil {
		log.Printf("telegram send failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// /reset
// ---------------------------------------------------------------------------

func (b *Bot) handleReset(ctx context.Context, msg *tgbotapi.Message, userID int64) {
	b.sendResetConfirmation(ctx, msg.Chat.ID, userID)
}

func (b *Bot) sendResetConfirmation(ctx context.Context, chatID int64, userID int64) {
	nonce, err := b.store.CreateDestructiveConfirmation(ctx, userID, "reset", destructiveConfirmationTTL)
	if err != nil {
		log.Printf("create reset confirmation failed: %v", err)
		b.reply(chatID, "I could not prepare a secure reset confirmation. Please try again.")
		return
	}
	confirmMsg := tgbotapi.NewMessage(chatID, `⚠️⚠️ RESET ALL DATA ⚠️⚠️

This will:
• Disconnect Canvas feed
• Disconnect Google Calendar
• Delete ALL settings, synced events, and pending actions
• Permanently delete Google Calendar events created and tracked by CanvasLink
• You'll need to go through the full onboarding again

Events CanvasLink does not own will never be touched.

Are you absolutely sure?`)
	confirmMsg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Yes, reset everything", CBPrefixConfirm+"reset|"+nonce),
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

func (b *Bot) handleConfirm(ctx context.Context, cq *tgbotapi.CallbackQuery, userID int64, payload string) {
	parts := strings.Split(payload, "|")
	if len(parts) != 2 {
		b.answerCallback(cq.ID, "This confirmation is no longer valid")
		b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
		return
	}
	action, nonce := parts[0], parts[1]
	switch action {
	case "disconnect_canvas", "disconnect_google", "reset":
	default:
		b.answerCallback(cq.ID, "Unknown confirmation")
		b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
		return
	}

	// Destructive actions share the same distributed per-user lock as feed and
	// calendar jobs. This prevents a create or delete from crossing a
	// disconnect/reset boundary.
	release, acquired, err := b.store.TryFeedSyncLock(ctx, userID)
	if err != nil {
		log.Printf("acquire destructive-action lock failed user=%d action=%s err=%v", userID, action, err)
		b.answerCallback(cq.ID, "Could not confirm safely; please try again")
		return
	}
	if !acquired {
		b.answerCallback(cq.ID, "A sync is finishing; tap again in a moment")
		return
	}
	defer func() {
		if err := release(); err != nil {
			log.Printf("release destructive-action lock failed user=%d action=%s err=%v", userID, action, err)
		}
	}()

	valid, err := b.store.ConsumeDestructiveConfirmation(ctx, userID, action, nonce)
	if err != nil {
		log.Printf("consume destructive confirmation failed user=%d action=%s err=%v", userID, action, err)
		b.answerCallback(cq.ID, "Could not confirm safely; please try again")
		return
	}
	if !valid {
		b.answerCallback(cq.ID, "This confirmation expired; request a new one")
		b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
		return
	}
	b.answerCallback(cq.ID, "Confirmed")
	b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)

	switch action {
	case "disconnect_canvas":
		if err := b.store.DisconnectCanvas(ctx, userID); err != nil {
			log.Printf("disconnect canvas failed: %v", err)
			b.reply(cq.Message.Chat.ID, "I could not disconnect Canvas. No partial reset was saved; please try again.")
			return
		}
		b.reply(cq.Message.Chat.ID, "✅ Canvas disconnected. Your Google Calendar events remain unchanged.\n\nSend /start to reconnect.")

	case "disconnect_google":
		if err := b.oauthServer.Disconnect(ctx, userID); err != nil {
			log.Printf("disconnect google failed: %v", err)
			b.reply(cq.Message.Chat.ID, "I could not disconnect Google Calendar. Please try again.")
			return
		}
		b.reply(cq.Message.Chat.ID, "✅ Google Calendar disconnected. Your existing events remain in your calendar.")

	case "reset":
		if err := b.wipeAndResetUser(ctx, userID); err != nil {
			log.Printf("reset user failed: %v", err)
			if errors.Is(err, errWipeNeedsGoogle) {
				b.reply(cq.Message.Chat.ID, "Reconnect the same Google account before wiping so I can safely remove the events CanvasLink created. Disconnecting alone intentionally keeps them.")
			} else if errors.Is(err, canvasGoogle.ErrEventOwnershipUnverified) {
				b.reply(cq.Message.Chat.ID, "I stopped the wipe because a tracked event could not be safely verified as CanvasLink-owned. Nothing unverified was deleted, and your local tracking was kept. Please review that event manually, then request /reset again.")
			} else {
				b.reply(cq.Message.Chat.ID, "I could not finish the wipe. Some verified events may already be gone, but your local tracking was kept. Request /reset again to retry safely.")
			}
			return
		}
		b.reply(cq.Message.Chat.ID, "✅ CanvasLink-created calendar events and all local data have been wiped.\n\nSend /start to begin again.")
	}
}

// ---------------------------------------------------------------------------
// Callback handler
// ---------------------------------------------------------------------------

func (b *Bot) handleCallback(ctx context.Context, cq *tgbotapi.CallbackQuery) {
	if cq == nil || cq.From == nil || cq.Message == nil || cq.Message.Chat == nil {
		return
	}
	if cq.Message.Chat.Type != "private" {
		b.answerCallback(cq.ID, "Please use CanvasLink in a private chat")
		return
	}
	userID := int64(cq.From.ID)
	if cq.Message.Chat.ID != userID {
		b.answerCallback(cq.ID, "This private action is not bound to your account")
		return
	}
	data := strings.TrimSpace(cq.Data)
	if strings.HasPrefix(data, "pl|") {
		b.handlePlannerCallback(ctx, cq, userID)
		return
	}

	switch {
	case data == CBPrefixOnboardGoogleYes:
		b.handleOnboardingGoogleYes(ctx, cq, userID)
	case data == CBPrefixOnboardGoogleNo:
		b.handleOnboardingGoogleNo(ctx, cq, userID)
	case strings.HasPrefix(data, CBPrefixOnboardMode):
		b.handleOnboardingMode(ctx, cq, userID, data)
	case strings.HasPrefix(data, CBPrefixSettingsHub):
		action := strings.TrimPrefix(data, CBPrefixSettingsHub)
		b.handleSettingsCallback(ctx, cq, userID, action)
	case strings.HasPrefix(data, CBPrefixConfirm):
		payload := strings.TrimPrefix(data, CBPrefixConfirm)
		b.handleConfirm(ctx, cq, userID, payload)
	case data == CBPrefixCancel:
		b.answerCallback(cq.ID, "Cancelled")
		b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
	case strings.HasPrefix(data, CBPrefixNoop):
		b.answerCallback(cq.ID, "")
	case strings.HasPrefix(data, CBPrefixCourse):
		courseID, err := b.resolveCourseCallback(ctx, userID, strings.TrimPrefix(data, CBPrefixCourse))
		if err != nil {
			log.Printf("resolve course callback failed: %v", err)
			b.answerCallback(cq.ID, "This module button is no longer valid")
			return
		}
		b.answerCallback(cq.ID, "")
		b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
	case data == CBPrefixModules:
		b.answerCallback(cq.ID, "")
		b.editModulesMenu(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID)
	case strings.HasPrefix(data, CBPrefixMode):
		parts := strings.Split(data, "|")
		if len(parts) != 3 && len(parts) != 4 {
			b.answerCallback(cq.ID, "Invalid")
			return
		}
		var (
			settingID      int64
			courseID       string
			assignmentType string
			mode           string
		)
		if len(parts) == 3 {
			var err error
			settingID, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil || settingID <= 0 {
				b.answerCallback(cq.ID, "Invalid")
				return
			}
			mode = parts[2]
			setting, err := b.store.GetCourseSettingByID(ctx, userID, settingID)
			if err != nil || setting == nil {
				log.Printf("resolve setting callback failed: %v", err)
				b.answerCallback(cq.ID, "This setting button is no longer valid")
				return
			}
			courseID = setting.CourseID
			assignmentType = setting.AssignmentType
		} else {
			// Backward compatibility for buttons sent before short IDs were introduced.
			courseID, assignmentType, mode = parts[1], parts[2], parts[3]
		}
		if mode != store.ModeAuto && mode != store.ModeActive && mode != store.ModeIgnore {
			b.answerCallback(cq.ID, "Invalid")
			return
		}
		releaseMode, modeLockAcquired, modeLockErr := b.store.TryFeedSyncLock(ctx, userID)
		if modeLockErr != nil {
			log.Printf("lock mode change failed user=%d err=%v", userID, modeLockErr)
			b.answerCallback(cq.ID, "Could not update this safely")
			return
		}
		if !modeLockAcquired {
			b.answerCallback(cq.ID, "A sync is finishing; try again in a moment")
			return
		}
		defer func() {
			if err := releaseMode(); err != nil {
				log.Printf("release mode-change lock failed user=%d err=%v", userID, err)
			}
		}()
		if mode == store.ModeAuto {
			connected, err := b.oauthServer.IsConnected(ctx, userID)
			if err != nil || !connected {
				b.answerCallback(cq.ID, "Connect Google before using Auto")
				return
			}
		}
		var err error
		if settingID > 0 {
			err = b.store.SetCourseTypeModeByID(ctx, userID, settingID, mode)
		} else {
			err = b.store.SetCourseTypeMode(ctx, userID, courseID, assignmentType, mode)
		}
		if err != nil {
			log.Printf("set mode failed: %v", err)
			b.answerCallback(cq.ID, "Failed")
			return
		}
		b.answerCallback(cq.ID, "Updated")
		b.editCourseMatrix(ctx, userID, cq.Message.Chat.ID, cq.Message.MessageID, courseID)
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
	if err != nil || pendingID <= 0 {
		b.answerCallback(cq.ID, "Invalid action")
		return
	}

	release, acquired, err := b.store.TryFeedSyncLock(ctx, userID)
	if err != nil {
		log.Printf("lock pending action failed user=%d err=%v", userID, err)
		b.answerCallback(cq.ID, "Could not verify this safely")
		return
	}
	if !acquired {
		b.answerCallback(cq.ID, "A sync is finishing; try again in a moment")
		return
	}
	defer func() {
		if err := release(); err != nil {
			log.Printf("release pending-action lock failed user=%d pending=%d err=%v", userID, pendingID, err)
		}
	}()

	pending, err := b.store.GetPendingAction(ctx, userID, pendingID)
	if err != nil || pending == nil {
		b.answerCallback(cq.ID, "Not found")
		return
	}
	pendingChatID, chatErr := strconv.ParseInt(pending.TelegramChatID, 10, 64)
	if chatErr != nil ||
		pendingChatID != cq.Message.Chat.ID ||
		!pending.TelegramMessageID.Valid ||
		pending.TelegramMessageID.Int64 != int64(cq.Message.MessageID) {
		b.answerCallback(cq.ID, "This card is outdated; use the newest one")
		b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
		return
	}
	currentMode, err := b.store.GetCourseTypeMode(
		ctx,
		userID,
		pending.CourseID,
		pending.AssignmentType,
	)
	if err != nil {
		log.Printf("check mode before pending action failed user=%d err=%v", userID, err)
		b.answerCallback(cq.ID, "Could not verify this safely")
		return
	}
	if currentMode != store.ModeReview {
		if _, cancelErr := b.store.CancelPendingActionByUID(ctx, userID, pending.CanvasEventUID); cancelErr != nil {
			log.Printf("invalidate mode-stale pending action failed user=%d err=%v", userID, cancelErr)
		}
		b.answerCallback(cq.ID, "This approval is no longer active")
		b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
		return
	}
	if pending.Status != store.PendingStatusPending {
		b.answerCallback(cq.ID, "This item was already handled")
		b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
		return
	}
	existing, err := b.store.GetSyncedEvent(ctx, userID, pending.CanvasEventUID)
	if err != nil {
		log.Printf("check current source before pending action failed: %v", err)
		b.answerCallback(cq.ID, "Could not verify this safely")
		return
	}
	if !pendingActionMatchesSyncedEvent(pending, existing) {
		if _, cancelErr := b.store.CancelPendingActionByUID(ctx, userID, pending.CanvasEventUID); cancelErr != nil {
			log.Printf("invalidate stale pending action failed user=%d err=%v", userID, cancelErr)
		}
		b.answerCallback(cq.ID, "This card is outdated; use the newest one")
		b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
		return
	}

	switch action {
	case "add":
		if syncedEventConfirmedInGoogle(existing) {
			if err := b.store.MarkPendingStatus(ctx, userID, pendingID, store.PendingStatusAdded); err != nil &&
				!errors.Is(err, store.ErrPendingAlreadyHandled) {
				log.Printf("recover pending status failed: %v", err)
				b.answerCallback(cq.ID, "Failed")
				return
			}
			b.answerCallback(cq.ID, "Already in Google Calendar")
			b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
			return
		}

		if !b.google.IsConfigured() {
			b.answerCallback(cq.ID, "Google not configured")
			return
		}
		connected, err := b.store.HasGoogleToken(ctx, userID)
		if err != nil {
			log.Printf("check Google token before pending add failed: %v", err)
			b.answerCallback(cq.ID, "Could not check Google connection")
			return
		}
		if !connected {
			b.answerCallback(cq.ID, "Connect Google first")
			return
		}

		eventID, err := b.google.DeterministicEventID(userID, pending.CanvasEventUID)
		if err != nil {
			log.Printf("build deterministic event ID for pending add failed: %v", err)
			b.answerCallback(cq.ID, "Could not queue calendar event")
			return
		}
		destination, err := b.google.EnsureDestination(ctx, userID)
		if err != nil {
			b.answerCallback(cq.ID, "Reconnect Google to prepare your CanvasLink calendar")
			return
		}
		_, err = b.store.QueuePendingCalendarAdd(ctx, store.PendingCalendarAddInput{
			TelegramUserID:   userID,
			PendingActionID:  pendingID,
			GoogleCalendarID: destination,
			GoogleEventID:    eventID,
			Description:      fmt.Sprintf("CanvasLink manual add\nCourse: %s\nType: %s", pending.CourseID, pending.AssignmentType),
			MaxAttempts:      12,
		})
		if err != nil {
			if errors.Is(err, store.ErrPendingAlreadyHandled) {
				b.answerCallback(cq.ID, "This item was already handled")
				b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
				return
			}
			if errors.Is(err, store.ErrNotFound) {
				b.answerCallback(cq.ID, "This Canvas item is no longer active")
				b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
				return
			}
			log.Printf("queue pending Google add failed: %v", err)
			b.answerCallback(cq.ID, "Could not queue Google add")
			return
		}
		b.answerCallback(cq.ID, "Queued for Google Calendar")
		b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
		b.reply(cq.Message.Chat.ID, "Queued for Google Calendar. CanvasLink will retry safely if Google is temporarily unavailable.")
	case "ignore":
		if err := b.store.MarkPendingStatus(ctx, userID, pendingID, store.PendingStatusIgnored); err != nil {
			if errors.Is(err, store.ErrPendingAlreadyHandled) {
				b.answerCallback(cq.ID, "This item was already handled")
				b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
				return
			}
			b.answerCallback(cq.ID, "Failed")
			return
		}
		b.answerCallback(cq.ID, "Ignored")
		b.clearInlineKeyboard(cq.Message.Chat.ID, cq.Message.MessageID)
		b.reply(cq.Message.Chat.ID, "Ignored. I will filter this item.")
	default:
		b.answerCallback(cq.ID, "Unknown action")
	}
}

// ---------------------------------------------------------------------------
// Module settings (existing matrix UI)
// ---------------------------------------------------------------------------

func (b *Bot) sendModulesMenu(ctx context.Context, userID int64, chatID int64) {
	courses, err := b.store.ListUserCourses(ctx, userID)
	if err != nil {
		log.Printf("list courses failed: %v", err)
		b.reply(chatID, "Could not load settings yet.")
		return
	}
	if len(courses) == 0 {
		b.reply(chatID, "No modules detected yet. Send /start to begin setup.")
		return
	}
	msg := tgbotapi.NewMessage(chatID, "CanvasLink Settings\n\nChoose a module:")
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
	msg := tgbotapi.NewEditMessageText(chatID, messageID, "CanvasLink Settings\n\nChoose a module:")
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
	googleConnected, err := b.oauthServer.IsConnected(ctx, userID)
	if err != nil {
		log.Printf("google status check failed: %v", err)
	}
	edit := tgbotapi.NewEditMessageText(chatID, messageID, msg.Text)
	edit.ReplyMarkup = courseSettingsKeyboard(courseID, settings, googleConnected)
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

func syncedEventConfirmedInGoogle(event *store.SyncedEvent) bool {
	return event != nil && event.GoogleConfirmed && event.GoogleEventID != ""
}

func pendingActionMatchesSyncedEvent(pending *store.PendingAction, event *store.SyncedEvent) bool {
	if pending == nil || event == nil ||
		event.Detached ||
		event.MissingSince != nil ||
		event.CanvasCancelled ||
		!event.RemovalEligible {
		return false
	}
	return pending.TelegramUserID == event.TelegramUserID &&
		pending.CanvasEventUID == event.CanvasEventUID &&
		pending.CourseID == event.CourseID &&
		pending.AssignmentType == event.AssignmentType &&
		pending.CanvasTitle == event.CanvasTitle &&
		pending.CanvasDueAt.Equal(event.CanvasDueAt) &&
		pending.CanvasAllDay == event.AllDay
}

func (b *Bot) reply(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("telegram send failed: %v", err)
	}
}

func (b *Bot) sendGoogleAuthButton(chatID int64, authURL, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("Connect Google Calendar", authURL),
		),
	)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("telegram send failed: %v", err)
	}
}

func (b *Bot) wipeAndResetUser(ctx context.Context, userID int64) error {
	candidates, err := b.store.ListCalendarOwnershipCandidates(ctx, userID)
	if err != nil {
		return fmt.Errorf("list tracked calendar ownership: %w", err)
	}

	if len(candidates) > 0 {
		connected, err := b.store.HasGoogleToken(ctx, userID)
		if err != nil {
			return fmt.Errorf("check google connection: %w", err)
		}
		if !connected || !b.google.IsConfigured() {
			return errWipeNeedsGoogle
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].CanvasEventUID != candidates[j].CanvasEventUID {
				return candidates[i].CanvasEventUID < candidates[j].CanvasEventUID
			}
			return candidates[i].GoogleEventID < candidates[j].GoogleEventID
		})
		for _, candidate := range candidates {
			if err := b.google.DeleteOwnedEvent(
				ctx,
				userID,
				candidate.GoogleCalendarID,
				candidate.GoogleEventID,
				candidate.CanvasEventUID,
			); err != nil {
				return fmt.Errorf("delete tracked Google event: %w", err)
			}
		}
	}

	if err := b.oauthServer.ResetUser(ctx, userID); err != nil {
		return fmt.Errorf("reset local data: %w", err)
	}
	return nil
}

func (b *Bot) clearInlineKeyboard(chatID int64, messageID int) {
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, tgbotapi.InlineKeyboardMarkup{})
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("clear inline keyboard failed: %v", err)
	}
}

func (b *Bot) hasOnboardingStatus(ctx context.Context, userID int64, expected string) bool {
	account, err := b.store.GetTelegramAccount(ctx, userID)
	if err != nil {
		log.Printf("get onboarding status failed user=%d err=%v", userID, err)
		return false
	}
	return account != nil && account.OnboardingStatus == expected
}

// ---------------------------------------------------------------------------
// Keyboard builders
// ---------------------------------------------------------------------------

func settingsMessage(chatID int64) tgbotapi.MessageConfig {
	msg := tgbotapi.NewMessage(chatID, "⚙️ CanvasLink Settings\n\nChoose an option:")
	msg.ReplyMarkup = settingsKeyboard()
	return msg
}

func settingsKeyboard() *tgbotapi.InlineKeyboardMarkup {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Upcoming", "pl|list|upcoming|0|0"),
			tgbotapi.NewInlineKeyboardButtonData("➕ Add task", "pl|input|add"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⏰ Reminders", "pl|rem|0"),
			tgbotapi.NewInlineKeyboardButtonData("🗓 Agendas", "pl|agenda"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🎨 Calendar", "pl|calendar"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📚 Module Sync Modes", CBPrefixSettingsHub+"modules"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔗 Google Calendar", CBPrefixSettingsHub+"google"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📡 Canvas Feed", CBPrefixSettingsHub+"feed"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🕒 Timezone", CBPrefixSettingsHub+"timezone"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🗑 Wipe / Reset", CBPrefixSettingsHub+"reset"),
		),
	)
	return &keyboard
}

func modulesKeyboard(courses []store.Course) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, c := range courses {
		label := c.CourseID
		if c.CourseName != "" && c.CourseName != c.CourseID {
			label = c.CourseName
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("%s%d", CBPrefixCourse, c.ID)),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 Back to Settings", CBPrefixSettingsHub+"main"),
	))
	markup := tgbotapi.NewInlineKeyboardMarkup(rows...)
	return &markup
}

func buildSettingsMatrixMessage(courseID string, settings []store.CourseSetting) tgbotapi.MessageConfig {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📚 %s\n\n", courseID))
	sb.WriteString("Tap a button below to change the sync mode for each type.\n\n")
	for _, s := range settings {
		emoji, label := displayMode(s.Mode)
		sb.WriteString(fmt.Sprintf("%s %s → %s\n", emoji, typeLabel(s.AssignmentType), label))
	}
	sb.WriteString("\nSelect a type to change its mode:")
	msg := tgbotapi.NewMessage(0, sb.String())
	return msg
}

func courseSettingsKeyboard(courseID string, settings []store.CourseSetting, googleConnected bool) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, s := range settings {
		emoji, modeLabel := displayMode(s.Mode)
		label := fmt.Sprintf("%s: %s %s", typeLabel(s.AssignmentType), emoji, modeLabel)
		targetMode := nextMode(s.Mode, googleConnected)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("%s%d|%s", CBPrefixMode, s.ID, targetMode)),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 Back to Modules", CBPrefixModules),
	))
	markup := tgbotapi.NewInlineKeyboardMarkup(rows...)
	return &markup
}

func (b *Bot) resolveCourseCallback(ctx context.Context, userID int64, payload string) (string, error) {
	settingID, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || settingID <= 0 {
		// Backward compatibility for already-sent course buttons.
		if strings.TrimSpace(payload) == "" {
			return "", errors.New("empty course callback")
		}
		return payload, nil
	}
	course, err := b.store.GetCourseBySettingID(ctx, userID, settingID)
	if err != nil {
		return "", err
	}
	if course == nil {
		return "", store.ErrNotFound
	}
	return course.CourseID, nil
}

func displayMode(mode string) (emoji, label string) {
	switch store.CanonicalMode(mode) {
	case store.ModeQuiet:
		return "🚀", "Auto"
	case store.ModeNotify:
		return "🔔", "Auto + Notify"
	case store.ModeReview:
		return "🔔", "Active"
	case store.ModeIgnore:
		return "🚫", "Ignore"
	default:
		return "❓", "Unknown"
	}
}

func nextMode(mode string, googleConnected bool) string {
	switch store.CanonicalMode(mode) {
	case store.ModeQuiet, store.ModeNotify:
		return store.ModeActive
	case store.ModeReview:
		return store.ModeIgnore
	case store.ModeIgnore:
		if googleConnected {
			return store.ModeAuto
		}
		return store.ModeActive
	default:
		return store.ModeActive
	}
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

func upcomingEventCount(events []canvas.Event) int {
	now := time.Now()
	count := 0
	for _, event := range events {
		if event.DueAt == nil {
			continue
		}
		if event.AllDay {
			if now.Before(event.DueAt.AddDate(0, 0, 1)) {
				count++
			}
			continue
		}
		if !event.DueAt.Before(now) {
			count++
		}
	}
	return count
}

// BuildSettingsMatrixMessage is the exported version for use by the sync worker.
func BuildSettingsMatrixMessage(courseID string, settings []store.CourseSetting) string {
	msg := buildSettingsMatrixMessage(courseID, settings)
	return msg.Text
}

func (b *Bot) registerCommands() error {
	cmds := botCommands()
	cfg := tgbotapi.NewSetMyCommands(cmds...)
	_, err := b.api.Request(cfg)
	return err
}
