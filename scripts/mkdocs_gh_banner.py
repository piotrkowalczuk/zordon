"""MkDocs hook: drop the GitHub-only canonical banner before rendering.

Every page under docs/ opens with a banner pointing GitHub's rendered copy
of the markdown back at zordon.io — GitHub serves docs/*.md itself and no
rel=canonical can be injected there.

On the built site that banner is redundant: the page already IS its
canonical URL and carries a proper <link rel="canonical">. Rendering it and
hiding it with CSS would leave hidden text plus a hidden self-link on all
25 pages, which is the shape Google's spam policy calls out. So strip it
from the source instead of styling it away — the HTML never contains it.
"""

import re

BANNER = re.compile(r'^<div class="gh-canonical">[^\n]*</div>\n+', re.MULTILINE)


def on_page_markdown(markdown, **_):
    return BANNER.sub("", markdown, count=1)
