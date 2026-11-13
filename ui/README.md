# Biralobase dashboard

The Biralobase superuser dashboard uses Shablon and Vite and retains PocketBase's dashboard extension and SDK compatibility.

Although it could be used independently, it is intended to be embedded and extended
as part of the Biralobase app executable (hence the `dist` directory and `embed.go` file).

The inherited UI kit and extension APIs are experimental. Existing `pbEvent` names, browser storage keys, SDK imports and `PB_` frontend environment variables remain compatible. The `PB_` variables contain public browser configuration; never put secrets in them.

## Black-cat identity

The SVG sources are in `public/images`: `logo.svg` for login and setup, `logo_white.svg` for the header, `favicon.svg` for the browser icon, and `wordmark.svg` for documentation. The legacy `logo_white.svg` filename now contains the black cat on a light badge, so the cat stays black and readable on dark surfaces. `favicon.png` and `favicon_prod.png` are 128-pixel renders of `favicon.svg` for both HTTP and HTTPS. Keep the generated assets in `dist` committed: Go embeds them directly.

The OAuth consent template in `internal/oauth/consent.go` includes the same cat as inline SVG, so it works without external images or changes to its restrictive content security policy. Update that copy when changing the mark.

PocketBase remains named in upstream attribution, official SDK imports and links to compatible API documentation. Product screens, help text, docs and release links use Biralobase.

## Development

Download the repository and run the appropriate console commands:

```sh
# install dependencies
npm ci

# start a dev server with hot reload at localhost:5173
npm run dev

# or generate production ready bundle in dist/ directory
npm run build
```
