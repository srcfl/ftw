"""Report whether the Home Assistant app has picked up the newest 0.x beta.

Every published 0.x beta is handed to srcfl/home-assistant-addons, which pins
it for the Home Assistant app within minutes. Nothing noticed when that
stopped: in November 2026 four betas in two days never reached the app,
because a test in that repository failed on every pin. A missing or expired
dispatch token, or a disabled workflow there, would look the same from here.

This compares the two ends, the releases here and the pin there, and nothing
in between. So it sees a stop whatever caused it.

Exit codes, matching scripts/check-debian-base.sh:
  0  the app pins the newest beta that has had time to land
  1  the app is behind
  2  the check could not run (bad input) -- never reported as "fine"
"""

from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import re
import sys


TAG = re.compile(r"^v0\.(\d+)\.(\d+)-beta\.(\d+)$")
FIRST_MINOR = 131  # the native 0.x line starts at v0.131.0-beta.1
RECEIPT = "ftw-native-release.json"
# A pin normally lands within minutes, and the app's pin workflow gives up
# after two hours. A beta younger than this may still be on its way.
GRACE = timedelta(hours=3)


def rank(tag):
    match = TAG.fullmatch(tag)
    if match is None or int(match[1]) < FIRST_MINOR:
        return None
    return tuple(int(part) for part in match.groups())


def published_betas(pages):
    """Published 0.x betas, oldest first: (rank, tag, published, has receipt)."""
    if not isinstance(pages, list) or any(not isinstance(page, list) for page in pages):
        raise ValueError("release list is not a list of pages")
    betas = []
    for page in pages:
        for release in page:
            if not isinstance(release, dict) or not isinstance(release.get("tag_name"), str):
                raise ValueError("release list has an invalid entry")
            order = rank(release["tag_name"])
            if order is None or release.get("draft"):
                continue
            published = datetime.fromisoformat(str(release.get("published_at")).replace("Z", "+00:00"))
            names = {asset.get("name") for asset in release.get("assets") or []}
            betas.append((order, release["tag_name"], published, RECEIPT in names))
    if not betas:
        raise ValueError("release list has no published 0.x beta")
    return sorted(betas)


def pinned(config):
    match = re.search(r'^version:\s*"?(0\.\d+\.\d+-beta\.\d+)"?\s*$', config, re.MULTILINE)
    if match is None or rank("v" + match[1]) is None:
        raise ValueError("the app's config.yaml pins no 0.x beta")
    return "v" + match[1]


def check(pages, config, now):
    """Return (behind, report)."""
    pin = pinned(config)
    betas = published_betas(pages)
    newer = [beta for beta in betas if beta[0] > rank(pin)]
    missed = [beta for beta in newer if now - beta[2] >= GRACE]
    newest = betas[-1]
    lines = [f"app pin:      {pin}",
             f"newest beta:  {newest[1]}, published {newest[2]:%Y-%m-%d %H:%M} UTC", ""]
    if missed:
        waited = int((now - missed[0][2]).total_seconds() // 3600)
        lines.append(f"The app is {len(missed)} beta(s) behind. The oldest has waited {waited} hours.")
        for _, tag, _, has_receipt in missed:
            lines.append(f"  {tag}" + ("" if has_receipt else f"  (no {RECEIPT}: the app cannot pin it)"))
    else:
        lines.append("The app pins the newest beta that has had time to land.")
    for _, tag, published, _ in newer:
        if now - published < GRACE:
            minutes = int((now - published).total_seconds() // 60)
            lines.append(f"{tag} is {minutes} min old and may still be on its way.")
    return bool(missed), "\n".join(lines)


def main(argv):
    if len(argv) != 3:
        print("usage: ha_app_pin.py RELEASE_PAGES_JSON APP_CONFIG_YAML", file=sys.stderr)
        return 2
    try:
        pages = json.loads(Path(argv[1]).read_text())
        behind, report = check(pages, Path(argv[2]).read_text(), datetime.now(timezone.utc))
    except Exception as error:  # 1 must only ever mean "behind", so no crash may exit 1
        print(f"could not compare the app pin with the releases: {error!r}", file=sys.stderr)
        return 2
    print(report)
    return 1 if behind else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
