#!/usr/bin/env python3
"""Check every link in every Markdown file of the course.

    python3 tools/check_links.py            # check everything
    python3 tools/check_links.py --local    # only links between files (no network)

Relative links must point at a file that exists. External links must answer
HTTP 200. Some sites refuse automated clients (403/429); those are listed
separately so a person can open them in a browser. A link that does not
answer, or answers with a server error, is tried twice more after a pause.

Uses only the Python standard library plus the `curl` command.
"""
import concurrent.futures
import pathlib
import re
import subprocess
import sys
import time

ROOT = pathlib.Path(__file__).resolve().parent.parent
LINK = re.compile(r"\]\(([^)\s]+)\)")
USER_AGENT = ("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
              "(KHTML, like Gecko) Chrome/126.0 Safari/537.36")


def strip_code(text):
    """Remove fenced code blocks so sample output is not parsed for links."""
    return re.sub(r"```.*?```", "", text, flags=re.S)


def collect():
    local, external = [], {}
    for md in sorted(ROOT.rglob("*.md")):
        if ".git" in md.parts:
            continue
        for target in LINK.findall(strip_code(md.read_text(encoding="utf-8"))):
            if target.startswith(("http://", "https://")):
                external.setdefault(target, md)
            elif not target.startswith(("#", "mailto:")):
                local.append((md, target))
    return local, external


def check_local(local):
    bad = []
    for md, target in local:
        path = (md.parent / target.split("#")[0]).resolve()
        if not path.exists():
            bad.append(f"{md.relative_to(ROOT)}: {target}")
    return bad


def fetch(url):
    out = subprocess.run(
        ["curl", "-sS", "-L", "--max-time", "40", "-A", USER_AGENT,
         "-o", "/dev/null", "-w", "%{http_code}", url.split("#")[0]],
        capture_output=True, text=True)
    return url, (out.stdout.strip() or "000")


def main():
    local, external = collect()
    failed = False

    bad_local = check_local(local)
    print(f"{len(local)} links between files, {len(bad_local)} broken")
    for line in bad_local:
        print("  BROKEN ", line)
        failed = True

    if "--local" in sys.argv:
        sys.exit(1 if failed else 0)

    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
        results = dict(pool.map(fetch, external))
    # A site that is asked for many pages at once may drop some connections. Ask again,
    # one page at a time and after a pause, before calling a link broken.
    for attempt in (1, 2):
        again = [u for u, code in results.items() if code == "000" or code.startswith("5")]
        for url in again:
            time.sleep(2 * attempt)
            results[url] = fetch(url)[1]
    results = sorted(results.items())
    ok = [u for u, code in results if code == "200"]
    blocked = [(u, code) for u, code in results if code in ("403", "429")]
    bad = [(u, code) for u, code in results if code not in ("200", "403", "429")]

    print(f"{len(external)} external links: {len(ok)} ok, "
          f"{len(blocked)} refused automated access, {len(bad)} failed")
    for url, code in blocked:
        print(f"  CHECK BY HAND ({code}) {url}   [{external[url].relative_to(ROOT)}]")
    for url, code in bad:
        print(f"  FAILED ({code}) {url}   [{external[url].relative_to(ROOT)}]")
        failed = True
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
