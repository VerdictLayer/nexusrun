#!/usr/bin/env python3
"""Render the GitHub walkthroughs as standalone website pages.

Requires the documentation-only Python Markdown package (pip install Markdown).
Usage: python3 scripts/render-guides.py /tmp/nexusrun-guide-site
"""

import argparse
import html
from html.parser import HTMLParser
from pathlib import Path
import posixpath
from urllib.parse import urlsplit

import markdown


ROOT = Path(__file__).resolve().parents[1]
PAGES = {"QUICKSTART.md": "get-started", "OFFLINE_DEMO.md": "offline-demo", "FLEET.md": "fleet-guide"}


class WebsiteLinks(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=False)
        self.output = []

    def handle_starttag(self, tag, attrs):
        rewritten = []
        for key, value in attrs:
            if key == "href" and value:
                url = urlsplit(value)
                if not url.scheme and not url.netloc and url.path.endswith(".md"):
                    path = posixpath.normpath(posixpath.join("docs", url.path))
                    page = PAGES.get(path.removeprefix("docs/")) if path.startswith("docs/") else None
                    value = ("/" + page) if page else ("https://github.com/verdictlayer/nexusrun/blob/main/" + path)
                    if url.fragment:
                        value += "#" + url.fragment
            rewritten.append(key if value is None else f'{key}="{html.escape(value, quote=True)}"')
        self.output.append("<" + tag + (" " + " ".join(rewritten) if rewritten else "") + ">")

    def handle_endtag(self, tag):
        self.output.append(f"</{tag}>")

    def handle_data(self, data):
        self.output.append(data)

    def handle_entityref(self, name):
        self.output.append(f"&{name};")

    def handle_charref(self, name):
        self.output.append(f"&#{name};")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path, help="directory for generated HTML")
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    template = (ROOT / "docs/site/guide-template.html").read_text()
    for source, slug in PAGES.items():
        document = (ROOT / "docs" / source).read_text()
        title = document.splitlines()[0].removeprefix("# ")
        renderer = markdown.Markdown(extensions=["fenced_code", "tables", "toc"], extension_configs={"toc": {"toc_depth": "2-3"}})
        body = WebsiteLinks()
        body.feed(renderer.convert(document))
        page = template.replace("{{TITLE}}", html.escape(title)).replace("{{SLUG}}", slug)
        page = page.replace("{{SOURCE}}", source).replace("{{TOC}}", renderer.toc).replace("{{BODY}}", "".join(body.output))
        (args.output / (slug + ".html")).write_text(page)
        print(args.output / (slug + ".html"))


if __name__ == "__main__":
    main()
