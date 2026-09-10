# CanvasLink landing page

A responsive static website built with HTML, CSS, and vanilla JavaScript. No build step or runtime dependencies. The interactive demo uses sample data and never connects to Telegram, Canvas, or Google accounts.

## Preview locally

From the repository root:

```sh
python3 -m http.server 4173 --directory docs
```

Open http://localhost:4173. Google Fonts supplies DM Sans and Manrope; system sans-serif fonts are used if the font service is unavailable.

## Publish on GitHub Pages

1. Push these files to the repository's `main` branch.
2. In the repository's **Settings → Pages**, set **Source** to **GitHub Actions**.
3. In **Actions**, run **Deploy landing page**, or push a change to `docs/` on `main`.
4. The workflow reports the published URL. For this repository it is expected to be https://yukemii.github.io/CanvasLink-Bot/.

The workflow uploads only `docs/`. No bot credentials, database, or Go server belong in this public directory. GitHub Pages hosts the landing page; the Telegram bot needs its own server.

Alternatively, use GitHub Pages' **Deploy from a branch** source with `main` and `/docs`, and remove `.github/workflows/pages.yml` so only one deployment method is active.

## Point the buttons to your live bot

The main buttons currently open [@CanvasLink_bot](https://t.me/CanvasLink_bot). The footer also includes a direct Telegram link.

Set `telegramUrl` in `config.js` to the public bot link:

```js
window.CANVASLINK_CONFIG = {
  telegramUrl: "https://t.me/YourBot",
};
```

The hero and closing buttons automatically become **Open in Telegram**. With an empty URL, they lead to the interactive demo. The feature section always keeps a link to the demo.

## Customize

- `index.html`: Copy, feature descriptions, setup guide, FAQ, and sharing metadata.
- `styles.css`: Palette, responsive layouts, typography, and CSS illustrations.
- `script.js`: Three-mode preview and sample assignment actions.
- `theme.js` / `theme.css`: Light/dark toggle, system preference detection, and coordinated dark surfaces. The chosen theme is saved locally and applied before styles load. If storage is blocked, the toggle still works for the current page.
- `assets/logo.png`: Supplied CanvasLink logo, used in the header, footer, bot preview, and favicon.
- `assets/social-logo.png`: 1200 × 630 social preview for LinkedIn and other platforms.
- `config.js`: Public bot URL only. Never put tokens or private feed URLs here.

If the repository is renamed or moved, update the GitHub links and absolute `og:url` / `og:image` values in `index.html`. After publication, share the Pages URL on LinkedIn. The social preview becomes available once the site is public.

The site supports keyboard navigation, visible focus indicators, native expandable FAQs, screen-reader status announcements, and reduced-motion preferences. The theme preference is stored locally in your browser. No analytics, cookies, or tracking scripts are included.
