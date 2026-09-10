"use strict";

const modes = {
  auto: {
    title: "Set it. Let it sync.",
    description:
      "New items sync silently to Google Calendar. No confirmation card, no extra tap. Ideal for the deadlines you always want to see.",
    kicker: "AUTO MODE · BEHIND THE SCENES",
    detail: "Added automatically to Google Calendar.",
    result:
      "Preview: Auto mode works silently. The bot does not send this card.",
  },
  active: {
    title: "A little nudge. The final say is yours.",
    description:
      "Get a Telegram confirmation card for each new item. Decide whether to add it to your calendar or ignore it.",
    kicker: "NEW CANVAS ASSIGNMENT",
    detail: "Want this on your Google Calendar?",
    result: "Your calendar, with you in control.",
  },
  ignore: {
    title: "Less noise. More focus.",
    description:
      "Skip this course and assignment type entirely. No new calendar events or review cards for items you choose to ignore.",
    kicker: "IGNORE MODE · BEHIND THE SCENES",
    detail: "Skipped. Nothing added to your calendar.",
    result: "Preview: Ignore mode does not send a Telegram card.",
  },
};

const actions = document.querySelector("#demo-actions");
const result = document.querySelector("#demo-result");
document.querySelectorAll("[data-mode]").forEach((button) => {
  button.addEventListener("click", () => {
    const mode = button.dataset.mode;
    const content = modes[mode];
    document
      .querySelectorAll("[data-mode]")
      .forEach((item) =>
        item.setAttribute("aria-pressed", String(item === button)),
      );
    document.querySelector("#mode-title").textContent = content.title;
    document.querySelector("#mode-description").textContent =
      content.description;
    document.querySelector("#demo-kicker").textContent = content.kicker;
    document.querySelector("#demo-detail").textContent = content.detail;
    document
      .querySelector("#demo-command")
      .replaceChildren(
        document.createTextNode(
          `CS2040 assignments → ${mode[0].toUpperCase() + mode.slice(1)} `,
        ),
      );
    const checks = document.createElement("span");
    checks.textContent = "✓✓";
    document.querySelector("#demo-command").append(checks);
    actions.hidden = mode !== "active";
    result.textContent = content.result;
  });
});

document.querySelectorAll("[data-demo-action]").forEach((button) => {
  button.addEventListener("click", () => {
    const added = button.dataset.demoAction === "add";
    actions.hidden = true;
    document.querySelector("#demo-detail").textContent = added
      ? "✓ Added to your Google Calendar."
      : "Skipped. This item won’t be added.";
    result.textContent = added
      ? "Demo only — no real calendar was changed. Try another mode!"
      : "Demo only — switch modes to explore, or select Active to try again.";
  });
});

const telegramUrl = window.CANVASLINK_CONFIG?.telegramUrl;
if (
  typeof telegramUrl === "string" &&
  /^https:\/\/t\.me\/[a-zA-Z0-9_]{5,32}$/.test(telegramUrl)
) {
  document.querySelectorAll("[data-bot-link]").forEach((link) => {
    link.href = telegramUrl;
    link.target = "_blank";
    link.rel = "noopener noreferrer";
    link.replaceChildren(document.createTextNode("Open in Telegram "));
    const arrow = document.createElement("span");
    arrow.textContent = "↗";
    arrow.setAttribute("aria-hidden", "true");
    link.append(arrow);
  });
}
