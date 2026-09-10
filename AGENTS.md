# CanvasLink project guidance

## Keep product surfaces in sync

When changing user-facing features or defaults, automatically review and update the affected surfaces in the same task:

- The bot's first `/start` message, returning-user message, onboarding explanations, `/help`, and registered command menu.
- Settings labels, confirmations, and explanations of what can be changed later.
- `README.md` command lists, defaults, limitations, and deployment instructions.
- The landing page in `docs/`: feature descriptions, onboarding steps, FAQs, interactive previews, and relevant sharing metadata.

Only change copy that is affected. Distinguish calendar sync modes from reminders, official deadlines from personal targets, and local completion from Canvas submission. Keep previews explicitly sample-only and consistent with the implementation, including defaults and opt-in features.

Check updated web sections in both themes and on mobile. Check bot messages stay within Telegram's text/callback limits. Avoid publishing claims that new backend features are already live unless the corresponding bot deployment has been verified. Existing logos, light/dark theme behavior, and unrelated edits should be preserved.
