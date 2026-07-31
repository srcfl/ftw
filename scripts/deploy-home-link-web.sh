#!/bin/bash
# Deploy web/home-link.html + home-link-session.js + components/theme.css to
# home.sourceful.energy. Run from the FTW repo root, on the commit you want
# live. Mirrors the existing hand-rolled release layout; the previous
# release directory is left in place so a rollback is one container recreate.
set -euo pipefail

INSTANCE=i-08351b29352efc64e
REGION=eu-central-1
IMAGE=sha256:5f5c8640aae01df9654968d946d8f1a56c497f1dd5c5cda4cf95ab7c14d58648
NETWORK=buzz-prod_buzz-net
CONTAINER=ftw-home-link-web

for f in web/home-link.html web/home-link-session.js web/components/theme.css; do
  test -f "$f" || { echo "run me from the repo root; missing $f" >&2; exit 1; }
done

SHA=$(git rev-parse --short=7 HEAD)
LABEL="${1:-$SHA}"
REL="/srv/ftw-home-link/releases/${LABEL}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

echo "==> packaging $LABEL"
mkdir -p "$WORK/bundle/components"
cp web/home-link.html web/home-link-session.js "$WORK/bundle/"
cp web/components/theme.css "$WORK/bundle/components/"
tar -czf "$WORK/web.tgz" -C "$WORK/bundle" .

run_ssm() { # run_ssm <script>
  local id
  python3 -c 'import json,sys; open(sys.argv[2],"w").write(json.dumps({"commands":[sys.argv[1]]}))' \
    "$1" "$WORK/params.json"
  id=$(aws ssm send-command --region "$REGION" --instance-ids "$INSTANCE" \
      --document-name AWS-RunShellScript --parameters "file://$WORK/params.json" \
      --query 'Command.CommandId' --output text)
  local status=Pending
  for _ in $(seq 1 60); do
    status=$(aws ssm get-command-invocation --region "$REGION" --command-id "$id" \
      --instance-id "$INSTANCE" --query 'Status' --output text 2>/dev/null || echo Pending)
    case "$status" in Success|Failed|Cancelled|TimedOut) break;; esac
    sleep 2
  done
  aws ssm get-command-invocation --region "$REGION" --command-id "$id" \
    --instance-id "$INSTANCE" \
    --query '[StandardOutputContent,StandardErrorContent]' --output text
  [ "$status" = Success ] || { echo "SSM step failed: $status" >&2; exit 1; }
}

echo "==> staging $REL"
B64=$(base64 < "$WORK/web.tgz" | tr -d '\n')
run_ssm "set -e
REL=$REL
sudo mkdir -p \$REL/web \$REL/bin
printf '%s' '$B64' | base64 -d > /tmp/home-link-web.tgz
sudo tar -xzf /tmp/home-link-web.tgz -C \$REL/web
CADDY=\$(sudo docker inspect $CONTAINER --format '{{range .Mounts}}{{if eq .Destination \"/ftw-bin/caddy\"}}{{.Source}}{{end}}{{end}}')
sudo cp \"\$CADDY\" \$REL/bin/caddy
sudo chmod 0755 \$REL/bin/caddy
cd \$REL/web && sudo sha256sum home-link.html home-link-session.js components/theme.css"

echo "==> expected checksums"
( cd "$WORK/bundle" && shasum -a 256 home-link.html home-link-session.js components/theme.css )

echo "==> swapping $CONTAINER onto $LABEL"
# The swap is the only destructive step. Capture where the running container
# points, and put it straight back if the new one does not answer, so a bad
# release cannot leave home.sourceful.energy without a web container.
run_ssm "set -e
start() { # start <web-mount> <caddy-mount>
  sudo docker run -d --name $CONTAINER --network $NETWORK --restart unless-stopped \\
    -v \"\$1\":/srv:ro -v \"\$2\":/ftw-bin/caddy:ro \\
    --entrypoint /ftw-bin/caddy $IMAGE \\
    file-server --root /srv --listen :8080 >/dev/null
}
OLD_WEB=\$(sudo docker inspect $CONTAINER --format '{{range .Mounts}}{{if eq .Destination \"/srv\"}}{{.Source}}{{end}}{{end}}')
OLD_BIN=\$(sudo docker inspect $CONTAINER --format '{{range .Mounts}}{{if eq .Destination \"/ftw-bin/caddy\"}}{{.Source}}{{end}}{{end}}')
echo \"previous release: \$OLD_WEB\"
sudo docker rm -f $CONTAINER >/dev/null
if ! start $REL/web $REL/bin/caddy; then
  echo 'new container failed to start; restoring previous release' >&2
  sudo docker rm -f $CONTAINER >/dev/null 2>&1 || true
  start \"\$OLD_WEB\" \"\$OLD_BIN\"
  exit 1
fi
ok=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if sudo docker exec $CONTAINER wget -q -O /dev/null http://127.0.0.1:8080/home-link.html 2>/dev/null; then ok=1; break; fi
  sleep 1
done
if [ \"\$ok\" -ne 1 ]; then
  echo 'new container did not serve home-link.html; restoring previous release' >&2
  sudo docker rm -f $CONTAINER >/dev/null 2>&1 || true
  start \"\$OLD_WEB\" \"\$OLD_BIN\"
  exit 1
fi
sudo docker ps --filter name=$CONTAINER --format '{{.Names}} {{.Status}}'"

echo "==> verifying live bytes against this checkout"
sleep 3
fail=0
for pair in "home-link.html:web/home-link.html" \
            "home-link-session.js:web/home-link-session.js" \
            "components/theme.css:web/components/theme.css"; do
  remote="https://home.sourceful.energy/${pair%%:*}"
  local_file="${pair##*:}"
  if curl -fsS --max-time 20 "$remote" | diff -q - "$local_file" >/dev/null; then
    echo "  ok   $remote"
  else
    echo "  DRIFT $remote"
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  echo "!! live content does not match this checkout" >&2
  exit 1
fi
echo "==> done: $LABEL is live"
