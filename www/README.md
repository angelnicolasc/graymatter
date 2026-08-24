# www — GrayMatter docs site

Static documentation site built with [Starlight](https://starlight.astro.build)
(Astro) and deployed to Cloudflare Workers as static assets.

The pages under `src/content/docs/guides/` are authored for the site. The
pages under `src/content/docs/reference/` are **generated at build time** by
`scripts/sync-docs.mjs` from the canonical docs in the repo root (`/docs`) —
edit those at the root, never the generated copies. The script also rewrites
relative links (internal docs links become site routes, code links point to
GitHub) and copies `.github/assets` into `public/assets`.

## Commands

```bash
npm install        # once
npm run dev        # local dev server
npm run build      # syncs reference docs, then builds into dist/
npm run deploy     # build + wrangler deploy (manual deploys)
```

## Deployment

CI deploys via Cloudflare Workers Builds (Git integration): every push to
`main` runs the build command and `npx wrangler deploy`. Non-production
branches get preview builds.

Config lives in `wrangler.jsonc` — assets-only, no Worker code. The Worker
`name` must match the Worker created in the Cloudflare dashboard.

The canonical `site` URL in `astro.config.mjs` feeds the sitemap and
`llms.txt`; update it after the first deploy or when attaching a custom
domain.
