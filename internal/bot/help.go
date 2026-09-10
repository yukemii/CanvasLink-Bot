package bot

import (
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// One catalog supplies both Telegram's command menu and /help.
func botCommands() []tgbotapi.BotCommand {
	return []tgbotapi.BotCommand{
		{Command: "start", Description: "Start or resume setup"},
		{Command: "help", Description: "Commands, defaults, and how CanvasLink works"},
		{Command: "settings", Description: "Sync modes, reminders, agendas, and calendar options"},
		{Command: "today", Description: "Today’s outstanding deadlines"},
		{Command: "week", Description: "The next 7 days"},
		{Command: "upcoming", Description: "Browse upcoming work and filter by course"},
		{Command: "completed", Description: "Completed items and undo"},
		{Command: "add", Description: "Create a personal task in Telegram"},
		{Command: "reminders", Description: "Reminder offsets, course/type overrides, and quiet hours"},
		{Command: "agenda", Description: "Schedule daily or weekly summaries"},
		{Command: "timezone", Description: "Show or change your local timezone"},
		{Command: "connect_google", Description: "Link Google Calendar (optional)"},
		{Command: "disconnect_canvas", Description: "Remove Canvas feed; keep Google events and personal tasks"},
		{Command: "disconnect_google", Description: "Remove Google authorization; keep calendar events"},
		{Command: "reset", Description: "Confirm a wipe of verified bot events and local data"},
		{Command: "cancel", Description: "Cancel current text input"},
	}
}

const welcomeMessage = `🎓 Welcome to CanvasLink!

Your Canvas deadlines, with a little less admin:
📋 Browse today, this week, or all upcoming work in Telegram.
⏰ Get a reminder 1 day before by default. Change offsets, set course/type overrides, or turn reminders off in /settings.
📅 Optionally sync to a separate CanvasLink Google Calendar.

Mark work Done, snooze reminders, add personal tasks, or set an earlier personal target. Daily/weekly scheduled agendas are optional and start off. Quiet hours default to 22:00–08:00 in your timezone.

First, connect your Canvas calendar feed:
1. Log into Canvas and open Calendar.
2. Choose Calendar Feed (usually on the right).
3. Copy the feed URL and paste it here in this private chat.

Then choose a default sync mode for your courses. You can toggle any course, assignment type, or reminder setting later in /settings. Need a guide? /help`

const returningMessage = `Welcome back! 👋

/today — What’s due today
/week — Your next 7 days
/upcoming — Browse work, mark Done, snooze, or set a personal target
/add — Add a personal task

Use /settings to change sync modes, reminders, scheduled agendas, or calendar preferences anytime. /help lists every command.`

func helpMessage() string {
	var b strings.Builder
	b.WriteString("🎓 CanvasLink help\n\nYour Canvas deadlines, reminders, and planner in a private Telegram chat.\n\n")
	for _, command := range botCommands() {
		b.WriteString("/" + command.Command + " — " + command.Description + "\n")
	}
	b.WriteString(`
⏰ Reminder defaults
On, 1 day before; quiet hours 22:00–08:00 in your timezone. Use /reminders for presets, multiple offsets (e.g. 1h, 1d, 1w), per-course/type overrides, or Off. Reminders also work without Google and before calendar approval. Ignored items are excluded. For all-day items, offsets count back from 09:00 on the due date.

🗓 Agendas
/today and /week always work after setup. Scheduled daily/weekly summaries start off; choose delivery times in /agenda.

📅 Calendar sync
Auto adds items silently; Active asks before adding; Ignore skips them. These modes are separate from reminders. Google is optional and defaults to a dedicated CanvasLink calendar. Change the destination, course colors, or title prefix in Settings → Calendar.

✅ Your planner
Done stops reminders locally; Undo restores future reminders. It does not submit work to Canvas or remove Google events. Snooze pauses for 1 hour. Personal targets change reminder timing, not official deadlines. Personal tasks stay in Telegram.

📡 Staying connected
Feed checks are periodic (normally hourly). I’ll report repeated connection failures and recovery; an invalid Google authorization prompts reconnection. Deadline-change messages show the previous and new dates. /upcoming shows the last successful Canvas check.

Change any course/type or notification preference anytime in /settings.`)
	return b.String()
}
