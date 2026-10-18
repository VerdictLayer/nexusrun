# Website walkthroughs

The GitHub Markdown files are the source for both website guides. To render
standalone pages with the site's colors, navigation, and copy buttons:

```sh
python3 -m pip install Markdown==3.7
python3 scripts/render-guides.py /tmp/nexusrun-guide-site
```

This is a documentation build dependency only; it is not used by NexusRun.
Styles, navigation, and copy buttons are embedded in each page. The template
also includes the website's Google Tag Manager container, `GTM-NVCFBTDC`,
with its script in the head and noscript iframe immediately after the body tag.

Copy `get-started.html` and `offline-demo.html` into the website source repo.
Serve them at `/get-started` and `/offline-demo`, with content type
`text/html; charset=utf-8`, matching the existing extensionless website routes.
Include both files in the site's deployment script and cache invalidation.
Link the homepage's Start section to `/get-started`.

Regenerate both pages whenever either walkthrough changes. Check mobile
widths, both color schemes, YAML copying, and internal links before publishing.
