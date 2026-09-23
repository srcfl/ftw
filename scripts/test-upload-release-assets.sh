#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/bin" "$fixture/remote" "$fixture/local"
export FTW_ASSET_FIXTURE="$fixture"
export GITHUB_REPOSITORY=example/fixture
export PATH="$fixture/bin:$PATH"

cat > "$fixture/bin/gh" <<'PY'
#!/usr/bin/env python3
import json
import os
from pathlib import Path
import shutil
import sys

root = Path(os.environ["FTW_ASSET_FIXTURE"])
args = sys.argv[1:]
assert args[0] == "release", args
assert args[args.index("--repo") + 1] == "example/fixture", args
if args[1] == "view":
    print(json.dumps({"assets": [{"name": p.name} for p in (root / "remote").iterdir()]}))
elif args[1] == "download":
    name = args[args.index("--pattern") + 1]
    destination = Path(args[args.index("--dir") + 1]) / name
    shutil.copyfile(root / "remote" / name, destination)
elif args[1] == "upload":
    source = Path(args[3])
    destination = root / "remote" / source.name
    assert not destination.exists(), "attempted to replace an asset"
    shutil.copyfile(source, destination)
else:
    raise SystemExit(f"unexpected gh call: {args}")
PY
chmod +x "$fixture/bin/gh"

printf 'first archive\n' > "$fixture/local/first.tar.gz"
printf 'second archive\n' > "$fixture/local/second.tar.gz"
# Partial publication: one file is already public, the other is missing.
cp "$fixture/local/first.tar.gz" "$fixture/remote/first.tar.gz"
bash "$root/scripts/upload-release-assets.sh" v0.131.0-beta.1 "$fixture/local/"*.tar.gz
cmp "$fixture/local/second.tar.gz" "$fixture/remote/second.tar.gz"
# A complete retry succeeds without replacing either file.
bash "$root/scripts/upload-release-assets.sh" v0.131.0-beta.1 "$fixture/local/"*.tar.gz
printf 'changed archive\n' > "$fixture/local/first.tar.gz"
printf 'new archive\n' > "$fixture/local/added.tar.gz"
if bash "$root/scripts/upload-release-assets.sh" v0.131.0-beta.1 "$fixture/local/added.tar.gz" "$fixture/local/first.tar.gz" > "$fixture/error" 2>&1; then
  echo "changed public bytes were accepted" >&2
  exit 1
fi
grep -Fq 'refusing to replace it' "$fixture/error"
test ! -e "$fixture/remote/added.tar.gz"
printf 'first archive\n' | cmp - "$fixture/remote/first.tar.gz"
echo "immutable release asset upload tests passed"
