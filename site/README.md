# ensemble — marketing site

A tiny, **zero-dependency** static site. `build.mjs` renders `./dist/` — plain
HTML/CSS/woff2/PNG with **relative paths** — which you can drop on any static web
server (Nginx, Caddy, GitLab/GitHub Pages, an S3 bucket, `file://` …). No runtime,
no framework, no `node_modules`.

## Build

Needs only Node (≥ 18):

```sh
cd site
node build.mjs          # → ./dist
# or: npm run build
```

Preview locally:

```sh
npm run serve           # builds, then serves ./dist on http://localhost:8000
```

Deploy: copy the contents of `./dist/` to your web root. That's it.

## Editing

- **Words** — everything on the page lives in [`content.mjs`](content.mjs)
  (hero, the four selling points, screenshots captions, steps, footer). Edit it and
  rebuild; no template changes needed.
- **Look** — [`src/assets/styles.css`](src/assets/styles.css) (dark theme, fonts,
  layout). Fonts are self-hosted in `src/assets/fonts/` (Fraunces, IBM Plex
  Sans/Mono) so the site has no external dependencies.
- **Screenshots** — `src/assets/img/` (the same images as `docs/user/`). Replace a
  file and rebuild to update a shot.
- **Structure** — [`build.mjs`](build.mjs) holds the HTML template + the build
  (render content → `dist/index.html`, copy `src/assets` → `dist/assets`).

## Layout

```
site/
  build.mjs            # the generator (Node stdlib only)
  content.mjs          # all copy — edit this
  src/assets/
    styles.css         # the design
    fonts/*.woff2       # self-hosted fonts
    img/*.png           # screenshots
  dist/                # build output (git-ignored)
```
