#!/usr/bin/env python3
"""Fetch Diff-XYZ samples and build testdata/diffxyz-30.jsonl (ARCHITECTURE.md §9).

Pulls the first 100 rows of the test split from the HuggingFace
datasets-server rows API (public, no auth), groups them by language, then
samples 6 rows per language with a fixed seed (20260913) for a fixed total
of 30 rows written as JSONL: {"language", "old_code", "new_code",
"search_replace"} per line. The file is committed; Go tests read it offline.

Determinism: the dataset is a fixed published snapshot and the rows API
returns rows in stable row_idx order; sampling uses random.Random(SEED) over
per-language lists sorted by row_idx, languages processed alphabetically.
Re-running the script yields a byte-identical file.

Only the Python standard library is used; HTTP goes through curl. On
networks where huggingface.co DNS is poisoned (TLS reset on connect), the
script resolves the real IP over DNS-over-HTTPS (dns.google) and retries via
curl --resolve. Both transports fetch the same bytes, so the output does not
depend on which one was used.

Probing notes (development): the /splits endpoint reports a single config
"default" with split "test" (1000 rows). Row fields include repo, commit,
path, lang, license, message, old_code, new_code, n_added, n_removed,
n_hunks, change_kind, udiff, udiff-h, udiff-l, search-replace. The
search-replace format is one or more blocks:

    <<<<<<< SEARCH
    old text
    =======
    new text
    >>>>>>> REPLACE

Blocks are separated by a blank line; the SEARCH text is a hunk-local
snippet of old_code, not necessarily the whole file.
"""

import json
import random
import subprocess
import sys
from pathlib import Path

DATASET = "JetBrains-Research/diff-xyz"
CONFIG = "default"
SPLIT = "test"
FETCH_ROWS = 100
PER_LANG = 6
SEED = 20260913

HOST = "datasets-server.huggingface.co"
ROWS_URL = (
    f"https://{HOST}/rows?dataset={DATASET.replace('/', '%2F')}"
    f"&config={CONFIG}&split={SPLIT}&offset=0&length={FETCH_ROWS}"
)
DOH_URL = f"https://dns.google/resolve?name={HOST}&type=A"

OUT_PATH = Path(__file__).resolve().parents[1] / "testdata" / "diffxyz-30.jsonl"


def _curl(args, url):
    return subprocess.run(
        ["curl", "-sS", "--fail", "--compressed", "-m", "90", *args, url],
        capture_output=True,
        text=True,
    )


def fetch_json(url):
    """GET url via curl and return the parsed JSON body.

    Tries a plain request first; if the connection is reset (poisoned DNS
    for huggingface.co on some networks), resolves the real A record over
    DoH and retries with curl --resolve.
    """
    r = _curl([], url)
    if r.returncode == 0 and r.stdout:
        return json.loads(r.stdout)

    doh = _curl([], DOH_URL)
    if doh.returncode != 0:
        sys.exit(f"error: direct request failed ({r.stderr.strip()}) and DoH lookup failed ({doh.stderr.strip()})")
    answers = json.loads(doh.stdout).get("Answer", [])
    ips = [a["data"] for a in answers if a.get("type") == 1]
    if not ips:
        sys.exit(f"error: request failed ({r.stderr.strip()}) and DoH returned no A records")
    for ip in ips:
        r2 = _curl(["--resolve", f"{HOST}:443:{ip}"], url)
        if r2.returncode == 0 and r2.stdout:
            return json.loads(r2.stdout)
    sys.exit(f"error: request failed via all transports: {r.stderr.strip()}")


def main():
    data = fetch_json(ROWS_URL)
    rows = data.get("rows", [])
    if len(rows) != FETCH_ROWS:
        sys.exit(f"error: expected {FETCH_ROWS} rows, got {len(rows)}")

    by_lang = {}
    for row in rows:
        r = row["row"]
        by_lang.setdefault(r["lang"], []).append((row["row_idx"], r))
    langs = sorted(by_lang)
    print(f"fetched {len(rows)} rows, languages: " + ", ".join(f"{k}={len(v)}" for k, v in by_lang.items()))

    if len(langs) != 5 or any(len(by_lang[l]) < PER_LANG for l in langs):
        sys.exit(f"error: need >= {PER_LANG} rows for each of 5 languages, got " + repr({l: len(by_lang[l]) for l in langs}))

    rng = random.Random(SEED)
    records = []
    for lang in langs:  # alphabetical: java, javascript, kotlin, python, rust
        idx_rows = sorted(by_lang[lang])  # by row_idx for determinism
        picks = rng.sample(idx_rows, PER_LANG)
        for row_idx, r in sorted(picks):
            records.append(
                {
                    "language": lang,
                    "old_code": r["old_code"],
                    "new_code": r["new_code"],
                    "search_replace": r["search-replace"],
                }
            )

    OUT_PATH.parent.mkdir(parents=True, exist_ok=True)
    with OUT_PATH.open("w", encoding="utf-8", newline="\n") as f:
        for rec in records:
            f.write(json.dumps(rec, ensure_ascii=True, separators=(",", ":")) + "\n")
    print(f"wrote {len(records)} samples to {OUT_PATH}")


if __name__ == "__main__":
    main()
