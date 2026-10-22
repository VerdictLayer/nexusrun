# Website walkthroughs

The GitHub Markdown files are the source for the website guides. To render
standalone pages with the site's colors, navigation, and copy buttons:

```sh
python3 -m pip install Markdown==3.7
python3 scripts/render-guides.py /tmp/nexusrun-guide-site
```

This is a documentation build dependency only; it is not used by NexusRun.
Styles, navigation, and copy buttons are embedded in each page. The template
also includes the website's Google Tag Manager container, `GTM-NVCFBTDC`,
with its script in the head and noscript iframe immediately after the body tag.
The same head includes the Google Analytics tag for `G-31LHFJYC2Y`.

Copy `get-started.html`, `offline-demo.html`, and `fleet-guide.html` into the website source repo.
Serve them at `/get-started`, `/offline-demo`, and `/fleet-guide`, with content type
`text/html; charset=utf-8`, matching the existing extensionless website routes.
Include all three files in the site's deployment script and cache invalidation.
Link the homepage's Start section to `/get-started`.

Regenerate the pages whenever a walkthrough changes. Check mobile
widths, both color schemes, YAML copying, and internal links before publishing.
