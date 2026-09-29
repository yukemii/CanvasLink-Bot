# CanvasLink website and Google verification

**Status:** the pre-launch website is published at https://site.dulie.app/canvaslink/.
The bot has been started locally for testing, not deployed to an always-on host.
Policies remain drafts until production arrangements are finalized.

The existing `dulie.app` registration can support CanvasLink as well as Dulie.
Creating `canvaslink.dulie.app` does not require buying another domain. DNS
routes hostnames, not paths: it cannot route `/canvaslink/` to another repository.

The CanvasLink policies identify **Ke Mi, Singapore**, with
**dulie.business@gmail.com** as the contact. They describe this repository's
implementation. Before public release, confirm the bot/database hosts, processing
locations, operational log and backup retention, and the manual deletion process;
update the privacy page to reflect those actual arrangements. Publication is not
Google verification or proof that the backend is deployed.

The current prepared bot links and homepage sharing metadata use
`https://site.dulie.app/canvaslink/`, following the requested path. To choose the
independent subdomain instead, update `privacyURL` and `termsURL` in
`internal/bot/help.go` and the absolute `og:url` / `og:image` in `docs/index.html`
before publication. Policy assets and navigation need no URL changes.

## Option A: independent subdomain (recommended)

| Purpose                  | URL / value                             |
| ------------------------ | --------------------------------------- |
| Homepage                 | `https://canvaslink.dulie.app/`         |
| Privacy                  | `https://canvaslink.dulie.app/privacy/` |
| Terms                    | `https://canvaslink.dulie.app/terms/`   |
| Google authorized domain | `dulie.app`                             |

1. In the CanvasLink repository's **Settings → Pages**, keep **GitHub Actions**
   as the source and save `canvaslink.dulie.app` as the custom domain.
   This is https://github.com/yukemii/CanvasLink-Bot/settings/pages for the current remote.
2. In Cloudflare's DNS settings for `dulie.app`, add a **CNAME** named
   `canvaslink`, targeting **`yukemii.github.io`**, with **DNS only** (grey cloud).
   The target has no scheme, path, or repository name. Preserve Dulie's `site`
   record, root records, email records, and verification TXT records.
3. If GitHub reports that another account has already verified the parent domain,
   resolve domain authorization with that account/organization; it may be simpler
   to host this Pages repository under the same owning organization as Dulie.
   Recheck the CNAME target if the repository owner changes.
4. Publish the reviewed changes and run **Deploy landing page**. Enable
   **Enforce HTTPS** in Pages once GitHub provisions the certificate.
   For this Actions deployment, a CNAME file alone does not set the custom domain.
5. Open all three URLs while signed out. Check policy links, logos, fonts,
   the Telegram button, mobile layout, and both themes.

The existing relative asset and policy links support a custom-domain root or a
project subdirectory. Keep absolute sharing metadata and bot policy links aligned
with the chosen public URL.

## Option B: under Dulie's website

Use these URLs if you choose to publish inside the Dulie website deployment:

- `https://site.dulie.app/canvaslink/`
- `https://site.dulie.app/canvaslink/privacy/`
- `https://site.dulie.app/canvaslink/terms/`

The local Dulie website repository is `/Users/yukemi/Landing-Page`. Its Vite
build copies `public/` into the website output. To use this option, incorporate
the CanvasLink website assets and HTML from this repository's `docs/` into
`public/canvaslink/` in that repository (exclude developer Markdown guides).
Publish through Dulie's existing build/deployment. Preserve Dulie's root pages,
privacy policy, and terms; CanvasLink's policies stay under `/canvaslink/`.

A copy of the current static files has been prepared locally at
`/Users/yukemi/Landing-Page/public/canvaslink/`. This is a snapshot, not an
automatic sync. It was published through Dulie’s website deployment in commit
`dcbb036`; homepage, privacy, and terms URLs returned HTTP 200 after deployment.
The independent CanvasLink Pages workflow does not publish to this path.

This requires coordinating updates between the repositories. Either maintain the
copy explicitly or add a reviewed build step that fetches a pinned CanvasLink
revision. A CanvasLink Pages deployment by itself will not update that path.
Update CanvasLink's sharing metadata and bot policy links to the same path.

`dulie.app/canvaslink/` remains possible later with either option. Its future
website/proxy must serve or redirect that path. There is no need to reserve the
path in DNS. Changing public URLs later requires updating OAuth branding links,
site metadata, bot links, and redirects from the old addresses.

## Google configuration (CanvasLink, separate from Dulie)

1. Use CanvasLink's production Cloud project and OAuth client. Do not replace
   Dulie's branding or credentials just because both apps share `dulie.app`.
2. In **Google Auth Platform → Branding**, use the chosen CanvasLink homepage,
   privacy, and terms URLs above. The privacy and terms fields must point to
   the actual policy pages, not three copies of the homepage URL.
3. Add `dulie.app` to **Authorized domains** (no scheme, subdomain, or path).
4. Verify the `dulie.app` Domain property in **Google Search Console** using an
   account with Owner/Editor access to CanvasLink's Google Cloud project. Existing
   domain verification can be reused if the account/project relationship is
   correct. Keep the verification DNS record. Google Search Console verification
   and GitHub Pages domain verification are separate.
5. Configure the actual backend OAuth redirect URI separately. GitHub Pages only
   hosts static files and cannot handle `/oauth/callback`. For example, once
   configured to point at the bot server, a separate hostname could serve
   `https://canvaslink-api.dulie.app/oauth/callback`. Match that URI exactly in
   Google and `CANVASLINK_OAUTH_REDIRECT_URL`. Do not point it at Pages.
6. Declare the three scopes in the README and review whether each is the narrowest
   scope needed. Complete brand verification/publication and sensitive-scope review
   with the working app, scope justifications, and an end-to-end demonstration.
7. Publish the website before deploying a bot version that links to these pages.
   Check real-account Google authorization and sync before announcing production
   availability. Deploying the website does not deploy the Go backend.

## Sources

- [Google domain and brand verification](https://developers.google.com/identity/protocols/oauth2/production-readiness/brand-verification)
- [Google data access and demonstration requirements](https://developers.google.com/identity/protocols/oauth2/production-readiness/sensitive-scope-verification)
- [Google API Services User Data Policy](https://developers.google.com/terms/api-services-user-data-policy)
- [GitHub Pages custom-domain setup](https://docs.github.com/en/pages/configuring-a-custom-domain-for-your-github-pages-site/managing-a-custom-domain-for-your-github-pages-site)
