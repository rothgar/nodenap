"""MkDocs hooks for the GitHub Pages site.

The docs are written to read well on GitHub, so some links point outside
docs/ (../collector/, ../charts/...) or at the YAML examples, which the site
doesn't render. Point those at the files on GitHub instead.
"""

import re

REPO = "https://github.com/rothgar/nodenap"
LINK = re.compile(r"\]\((\.\./[^)\s#]*|examples/[^)\s#]*)(#[^)\s]*)?\)")


def _github(path: str) -> str:
    if path.startswith("../"):
        path = path[3:]
    else:
        path = "docs/" + path
    kind = "tree" if path.endswith("/") or "." not in path.rsplit("/", 1)[-1] else "blob"
    return f"{REPO}/{kind}/main/{path.rstrip('/')}"


def on_page_markdown(markdown, page, config, files, **kwargs):
    return LINK.sub(lambda m: f"]({_github(m.group(1))}{m.group(2) or ''})", markdown)
