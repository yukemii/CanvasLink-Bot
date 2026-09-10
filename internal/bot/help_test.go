package bot

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func TestWelcomeAndHelpFitTelegramLimits(t *testing.T) {
	for name, message := range map[string]string{"welcome": welcomeMessage, "returning": returningMessage, "help": helpMessage()} {
		if n := len(utf16.Encode([]rune(message))); n > 4096 {
			t.Errorf("%s is %d UTF-16 units; Telegram permits 4096", name, n)
		}
	}
	seen := map[string]bool{}
	for _, command := range botCommands() {
		if seen[command.Command] {
			t.Errorf("duplicate command %q", command.Command)
		}
		seen[command.Command] = true
		if n := len([]rune(command.Description)); n < 1 || n > 256 {
			t.Errorf("description for %s exceeds Telegram limits", command.Command)
		}
		if !strings.Contains(helpMessage(), "/"+command.Command+" — ") {
			t.Errorf("menu command %s missing from help", command.Command)
		}
	}
	if !seen["help"] {
		t.Fatal("help is missing from the command menu")
	}
}
