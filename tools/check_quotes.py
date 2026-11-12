#!/usr/bin/env python3
"""Check that quoted text in the chapters really appears in the linked source.

    python3 tools/check_quotes.py                 # every chapter
    python3 tools/check_quotes.py path/to/file.md # one file

For every double-quoted passage of five words or more, the script looks at
the links in the same paragraph (or the attribution line just below a block
quote), downloads those pages, and checks that the quoted words appear in at
least one of them. Passages that cannot be confirmed are listed for a person
to check; some always will be, because a page is rendered by JavaScript, is a
two-column PDF, or refuses automated clients.

Comparison ignores case, spacing and punctuation, and treats "…" and
"[inserted words]" inside a quotation as gaps.

Uses the Python standard library plus `curl` (and `pdftotext` for PDFs).
"""
import hashlib
import html.parser
import pathlib
import re
import subprocess
import sys
import tempfile
import unicodedata

ROOT = pathlib.Path(__file__).resolve().parent.parent
CACHE = pathlib.Path(tempfile.gettempdir()) / "course-quote-cache"
USER_AGENT = ("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
              "(KHTML, like Gecko) Chrome/126.0 Safari/537.36")

LINK = re.compile(r"\]\((https?://[^)\s]+)\)")
MIN_WORDS = 5
MIN_FRAGMENT = 12  # characters, after squashing


def quotations(prose):
    """Return the double-quoted passages in a paragraph.

    Quote marks are paired in order (first with second, third with fourth),
    so a short quotation early in a paragraph cannot throw the rest off.
    """
    pieces = prose.split('"')
    return [q for q in pieces[1::2] if len(q.split()) >= MIN_WORDS]


class TextExtractor(html.parser.HTMLParser):
    def __init__(self):
        super().__init__()
        self.parts, self.skip = [], 0

    def handle_starttag(self, tag, attrs):
        if tag in ("script", "style", "noscript"):
            self.skip += 1

    def handle_endtag(self, tag):
        if tag in ("script", "style", "noscript") and self.skip:
            self.skip -= 1

    def handle_data(self, data):
        if not self.skip:
            self.parts.append(data)


def squash(text):
    """Lower-case and keep only letters and digits.

    Text is first decomposed (NFKD), so that a typeset ligature such as the
    single character "ﬁ" in a PDF compares equal to the letters "fi", and an
    accented letter to its base letter.
    """
    text = unicodedata.normalize("NFKD", text)
    return re.sub(r"[^a-z0-9]+", "", text.lower())


def page_text(url):
    """Return the squashed text of a page, or None if it cannot be fetched."""
    CACHE.mkdir(exist_ok=True)
    url = url.split("#")[0]
    raw = CACHE / (hashlib.sha1(url.encode()).hexdigest()[:20] + ".raw")
    if not raw.exists():
        code = subprocess.run(
            ["curl", "-sS", "-L", "--max-time", "60", "-A", USER_AGENT,
             "-o", str(raw), "-w", "%{http_code}", url],
            capture_output=True, text=True).stdout.strip()
        if code != "200":
            raw.unlink(missing_ok=True)
            return None
    data = raw.read_bytes()
    if data[:4] == b"%PDF":
        out = subprocess.run(["pdftotext", str(raw), "-"], capture_output=True)
        return squash(out.stdout.decode("utf-8", "replace"))
    parser = TextExtractor()
    parser.feed(data.decode("utf-8", "replace"))
    return squash(" ".join(parser.parts))


def fragments(quote):
    """Split a quotation at ellipses and [insertions]; squash each piece."""
    pieces = re.split(r"…|\.\.\.|\[[^\]]*\]", quote)
    return [s for s in (squash(p) for p in pieces) if len(s) >= MIN_FRAGMENT]


def blocks(md_text):
    """Yield (first line number, text) for each paragraph outside code blocks.

    A block quote and the attribution line that follows it count as one
    paragraph, as do the rows of a table. A paragraph that ends with a colon
    is joined to the list, table or quotation it introduces.
    """
    lines = md_text.split("\n")
    in_code, start, buf = False, 0, []
    for i, line in enumerate(lines, 1):
        if line.strip().startswith("```"):
            in_code = not in_code
            line = ""
        if in_code:
            line = ""
        if line.strip() == "" or line.strip() == ">":
            if line.strip() == ">" and buf:
                buf.append(" ")
                continue
            if buf and buf[-1].rstrip().endswith(":"):
                continue  # an introduction: keep collecting what it introduces
            if buf:
                yield start, " ".join(buf)
                buf = []
            continue
        if not buf:
            start = i
        buf.append(line)
    if buf:
        yield start, " ".join(buf)


def check(md_path):
    confirmed, unconfirmed, no_link = 0, [], 0
    text = md_path.read_text(encoding="utf-8")
    for line_no, block in blocks(text):
        # Inline code is not a quotation.
        prose = re.sub(r"`[^`]*`", lambda m: m.group(0).replace('"', "'"), block)
        quotes = quotations(prose)
        if not quotes:
            continue
        urls = LINK.findall(block)
        if not urls:
            no_link += len(quotes)
            continue
        pages = {u: page_text(u) for u in dict.fromkeys(urls)}
        for q in quotes:
            parts = fragments(q)
            if not parts:
                continue
            if any(p is not None and all(f in p for f in parts) for p in pages.values()):
                confirmed += 1
            else:
                unreachable = [u for u, p in pages.items() if p is None]
                unconfirmed.append((line_no, q, unreachable))
    return confirmed, unconfirmed, no_link


def main():
    targets = [pathlib.Path(a).resolve() for a in sys.argv[1:]] or sorted(
        p for p in ROOT.rglob("*.md") if ".git" not in p.parts)
    total_ok = total_bad = total_nolink = 0
    for md in targets:
        ok, bad, no_link = check(md)
        total_ok, total_bad, total_nolink = total_ok + ok, total_bad + len(bad), total_nolink + no_link
        if ok or bad:
            print(f"{md.relative_to(ROOT)}: {ok} confirmed, {len(bad)} to check by hand")
        for line_no, quote, unreachable in bad:
            short = quote if len(quote) < 110 else quote[:107] + "..."
            why = "  (page could not be fetched)" if unreachable else ""
            print(f"    line {line_no}: \"{short}\"{why}")
    print(f"\n{total_ok} quotations confirmed in their linked source; "
          f"{total_bad} to check by hand; "
          f"{total_nolink} quoted passages had no link in the same paragraph.")


if __name__ == "__main__":
    main()
