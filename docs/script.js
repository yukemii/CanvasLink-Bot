"use strict";

const modes = {
  auto: {
    title: "Set it. Let it sync.",
    description:
      "New items sync silently to your chosen Google Calendar. No approval tap needed. Deadline reminders are configured separately and start one day before.",
    kicker: "AUTO MODE · BEHIND THE SCENES",
    detail: "Added automatically to Google Calendar.",
    result:
      "Preview: Auto does not send this sync card. Separate reminders still apply.",
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
      "Skip this course and assignment type. No new calendar events, review cards, or reminders for ignored items.",
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

// Sample-only planner: completion is shared between its reminder and agenda views.
const plannerPreviewState = { done: false, snoozed: false };
const plannerPreviewStatus = document.querySelector("#planner-preview-status");
const previewDue = document.querySelector("#preview-due");
const previewDone = document.querySelector("#preview-done");
const previewSnooze = document.querySelector("#preview-snooze");
const previewOffset = document.querySelector("#preview-offset");
function renderPlannerPreview() {
  const labels = {
    hour: "Due in one hour",
    day: "Due tomorrow · 17:00",
    week: "Due in one week · 17:00",
  };
  previewDue.textContent = labels[previewOffset.value];
  previewDone.textContent = plannerPreviewState.done
    ? "Undo Done ↩"
    : "Mark Done ✓";
  previewDone.setAttribute("aria-pressed", String(plannerPreviewState.done));
  previewSnooze.disabled = plannerPreviewState.done;
  previewSnooze.textContent = plannerPreviewState.snoozed
    ? "Snoozed for 1 hour ✓"
    : "Snooze 1 hour";
  document.querySelector("#preview-agenda-assignment").hidden =
    plannerPreviewState.done;
  plannerPreviewStatus.textContent = plannerPreviewState.done
    ? "Done in CanvasLink. Reminders stop and this item leaves your outstanding agenda. Nothing is submitted to Canvas."
    : plannerPreviewState.snoozed
      ? "Snoozed for one hour in this preview. Real reminders also respect your quiet hours."
      : `Reminder set to ${previewOffset.value === "hour" ? "one hour" : previewOffset.value === "week" ? "one week" : "one day"} before. Change or disable it anytime in Settings.`;
}
document.querySelectorAll("[data-planner-view]").forEach((button) => {
  button.addEventListener("click", () => {
    const agenda = button.dataset.plannerView === "agenda";
    document.querySelector("#reminder-preview").hidden = agenda;
    document.querySelector("#agenda-preview").hidden = !agenda;
    document
      .querySelectorAll("[data-planner-view]")
      .forEach((item) =>
        item.setAttribute("aria-pressed", String(item === button)),
      );
  });
});
previewOffset.addEventListener("change", renderPlannerPreview);
previewDone.addEventListener("click", () => {
  plannerPreviewState.done = !plannerPreviewState.done;
  plannerPreviewState.snoozed = false;
  renderPlannerPreview();
});
previewSnooze.addEventListener("click", () => {
  plannerPreviewState.snoozed = true;
  renderPlannerPreview();
});
renderPlannerPreview();
