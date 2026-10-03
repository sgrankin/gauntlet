#!/usr/bin/env python3
"""Check repository-relative Markdown links without accessing external URLs."""
from pathlib import Path
import re
import sys
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[1]
LINK = re.compile(r'!?\[[^\]]*\]\(([^\s)]+)(?:\s+"[^"]*")?\)', re.MULTILINE)


def prose(text):
    return re.sub(r'^```[^\n]*\n.*?^```\s*$', '', text, flags=re.MULTILINE | re.DOTALL)


def main():
    errors = []
    pages = [ROOT / 'README.md', ROOT / 'DESIGN.md', ROOT / 'AGENTS.md']
    pages.extend(sorted((ROOT / 'docs').rglob('*.md')))
    for page in pages:
        for match in LINK.finditer(prose(page.read_text())):
            url = urlsplit(match[1].strip('<>'))
            if url.scheme or url.netloc or not url.path:
                continue
            target = (page.parent / unquote(url.path)).resolve()
            if not target.exists():
                errors.append(f'{page.relative_to(ROOT)}: missing {match[1]}')
    if errors:
        print('\n'.join(errors), file=sys.stderr)
        return 1
    print(f'Checked local links in {len(pages)} Markdown files.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
