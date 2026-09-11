#!/usr/bin/env bash
set -euo pipefail

root="${FTW_RELEASE_TEST_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
beta="${root}/.github/workflows/beta.yml"
release="${root}/.github/workflows/release.yml"
assets="${root}/.github/workflows/release-assets.yml"
compose="${root}/docker-compose.yml"
compose_macos="${root}/docker-compose.macos.yml"
dockerfile="${root}/Dockerfile"
core_build="${root}/scripts/build-core.sh"
release_guard="${root}/scripts/check-stable-release.py"

for workflow in "${beta}" "${release}" "${assets}"; do
  if grep -Eq 'SOURCEFUL_GHCR_(USER|TOKEN)' "${workflow}"; then
    echo "canonical GHCR writes must use the workflow GITHUB_TOKEN: ${workflow}" >&2
    exit 1
  fi
done
grep -Fq 'username: ${{ github.actor }}' "${beta}"
grep -Fq 'password: ${{ secrets.GITHUB_TOKEN }}' "${beta}"
grep -Fq 'CANONICAL_GHCR_USER: ${{ github.actor }}' "${assets}"
grep -Fq 'CANONICAL_GHCR_TOKEN: ${{ secrets.GITHUB_TOKEN }}' "${assets}"
grep -Fq 'LEGACY_GHCR_TOKEN' "${beta}"
grep -Fq 'LEGACY_GHCR_TOKEN' "${release}"
grep -Fq 'LEGACY_GHCR_TOKEN' "${assets}"
if grep -Fq 'LEGACY_GHCR_TOKEN || secrets.GITHUB_TOKEN' "${beta}" || \
   grep -Fq 'LEGACY_GHCR_TOKEN || secrets.GITHUB_TOKEN' "${release}" || \
   grep -Fq 'LEGACY_GHCR_TOKEN || secrets.GITHUB_TOKEN' "${assets}"; then
  echo "compatibility GHCR must fail early instead of falling back to a token that cannot write the personal namespace" >&2
  exit 1
fi
grep -Fq 'password: ${{ secrets.LEGACY_GHCR_TOKEN }}' "${beta}"
grep -Fq 'COMPATIBILITY_GHCR_TOKEN: ${{ secrets.LEGACY_GHCR_TOKEN }}' "${assets}"
grep -Fq 'bash scripts/check-ghcr-write-access.sh srcfl/ftw srcfl/ftw-updater' "${beta}"
grep -Fq 'bash scripts/check-ghcr-write-access.sh frahlg/forty-two-watts frahlg/forty-two-watts-updater' "${beta}"
grep -Fq 'bash scripts/check-ghcr-write-access.sh srcfl/ftw srcfl/ftw-updater' "${release}"
grep -Fq 'bash scripts/check-ghcr-write-access.sh frahlg/forty-two-watts frahlg/forty-two-watts-updater' "${release}"
grep -Fq 'bash scripts/check-ghcr-write-access.sh srcfl/ftw srcfl/ftw-updater' "${assets}"
grep -Fq 'bash scripts/check-ghcr-write-access.sh frahlg/forty-two-watts frahlg/forty-two-watts-updater' "${assets}"
grep -A3 '^  tag:$' "${beta}" | grep -Fq 'needs: registry'
grep -A3 '^  meta:$' "${assets}" | grep -Fq 'needs: registry'

beta_registry="$(grep -n '^  registry:$' "${beta}" | cut -d: -f1)"
beta_tag="$(grep -n '^  tag:$' "${beta}" | cut -d: -f1)"
stable_registry="$(grep -n '^  registry:$' "${assets}" | cut -d: -f1)"
stable_meta="$(grep -n '^  meta:$' "${assets}" | cut -d: -f1)"
stable_write_check="$(grep -n 'name: Verify compatibility package writes before stable release state' "${release}" | cut -d: -f1)"
stable_tag="$(grep -n 'name: Tag and prepare draft GitHub Release' "${release}" | cut -d: -f1)"
if [ -z "${beta_registry}" ] || [ -z "${beta_tag}" ] || \
   [ "${beta_registry}" -ge "${beta_tag}" ]; then
  echo "beta registry credentials must be checked before the immutable tag is created" >&2
  exit 1
fi
if [ -z "${stable_registry}" ] || [ -z "${stable_meta}" ] || \
   [ "${stable_registry}" -ge "${stable_meta}" ]; then
  echo "stable registry credentials must be checked before release metadata is derived" >&2
  exit 1
fi
if [ -z "${stable_write_check}" ] || [ -z "${stable_tag}" ] || \
   [ "${stable_write_check}" -ge "${stable_tag}" ]; then
  echo "stable package writes must be checked before tag or draft release creation" >&2
  exit 1
fi

release_inventory='[[
  {"tag_name":"v2.0.0","draft":true,"prerelease":false,"published_at":null},
  {"tag_name":"v2.1.0","draft":false,"prerelease":false,"published_at":"2026-08-17T00:00:00Z"},
  {"tag_name":"v99.0.0","draft":true,"prerelease":false,"published_at":null},
  {"tag_name":"v98.0.0","draft":false,"prerelease":true,"published_at":"2026-08-17T00:00:00Z"}
]]'
if printf '%s' "${release_inventory}" | python3 "${release_guard}" order v2.0.0 2>/dev/null; then
  echo "an abandoned old draft could downgrade a newer public stable" >&2
  exit 1
fi
printf '%s' "${release_inventory}" | python3 "${release_guard}" order v2.2.0
if printf '{}' | python3 "${release_guard}" order v2.2.0 2>/dev/null; then
  echo "malformed release inventory did not fail closed" >&2
  exit 1
fi

required_asset_names=(
  ftw-promotion-receipt.json os_list.json
  ftw-linux-amd64.tar.gz ftw-linux-amd64.tar.gz.sha256
  forty-two-watts-linux-amd64.tar.gz forty-two-watts-linux-amd64.tar.gz.sha256
  ftw-linux-arm64.tar.gz ftw-linux-arm64.tar.gz.sha256
  forty-two-watts-linux-arm64.tar.gz forty-two-watts-linux-arm64.tar.gz.sha256
  ftw-windows-amd64.zip ftw-windows-amd64.zip.sha256
  forty-two-watts-windows-amd64.zip forty-two-watts-windows-amd64.zip.sha256
)
release_assets_json="$(
  printf '%s\n' "${required_asset_names[@]}" | jq -Rn --arg tag v2.2.0 '
    {tagName: $tag, isDraft: true, isPrerelease: false, publishedAt: null,
     assets: [inputs | {name: ., state: "uploaded", size: 1}]}
  '
)"
printf '%s' "${release_assets_json}" | python3 "${release_guard}" assets v2.2.0
unexpected_assets="$(jq '.assets += [{name: "stale-build.txt", state: "uploaded", size: 1}]' <<<"${release_assets_json}")"
if printf '%s' "${unexpected_assets}" | python3 "${release_guard}" assets v2.2.0 2>/dev/null; then
  echo "an unexpected stale release asset passed the exact allow-list" >&2
  exit 1
fi
unnamed_assets="$(jq '.assets[-1].name = null' <<<"${release_assets_json}")"
if printf '%s' "${unnamed_assets}" | python3 "${release_guard}" assets v2.2.0 2>/dev/null; then
  echo "an unnamed release asset passed the exact allow-list" >&2
  exit 1
fi

release_test_tmp="$(mktemp -d)"
trap 'rm -rf "${release_test_tmp}"' EXIT
expected_notes="${release_test_tmp}/release-notes.md"
printf 'FTW 2.2.0\n\n<!-- ftw-state-schema:7 -->\n' > "${expected_notes}"
fresh_draft="$(jq -n --arg tag v2.2.0 --rawfile body "${expected_notes}" \
  '{tagName: $tag, name: $tag, body: $body, isDraft: true, isPrerelease: false, publishedAt: null}')"
printf '%s' "${fresh_draft}" | python3 "${release_guard}" draft v2.2.0 7 "${expected_notes}"
stale_draft="$(jq '.body = "old notes\n\n<!-- ftw-state-schema:6 -->\n"' <<<"${fresh_draft}")"
if printf '%s' "${stale_draft}" | python3 "${release_guard}" draft v2.2.0 7 "${expected_notes}" 2>/dev/null; then
  echo "a stale reused draft body or state-schema marker passed verification" >&2
  exit 1
fi
dual_marker_notes="${release_test_tmp}/dual-marker-notes.md"
printf '<!-- ftw-state-schema:6 -->\nFTW 2.2.0\n\n<!-- ftw-state-schema:7 -->\n' > "${dual_marker_notes}"
dual_marker_draft="$(jq -n --arg tag v2.2.0 --rawfile body "${dual_marker_notes}" \
  '{tagName: $tag, name: $tag, body: $body, isDraft: true, isPrerelease: false, publishedAt: null}')"
if printf '%s' "${dual_marker_draft}" | \
  python3 "${release_guard}" draft v2.2.0 7 "${dual_marker_notes}" 2>/dev/null; then
  echo "a stale first schema marker passed beside the expected marker" >&2
  exit 1
fi

grep -Fq 'VERSION=${{ needs.tag.outputs.runtime_version }}' "${beta}"
grep -Fq 'CANDIDATE_TAG=${{ needs.tag.outputs.version }}' "${beta}"
grep -Fq 'org.opencontainers.image.version=${{ needs.tag.outputs.oci_version }}' "${beta}"
grep -Fq 'created: ${{ steps.version.outputs.created }}' "${beta}"
grep -Fq 'group: beta-release-channel' "${beta}"
grep -Fq 'name: guard immutable beta candidate' "${beta}"
grep -Fq 'receipt_found: ${{ steps.guard.outputs.receipt_found }}' "${beta}"
grep -Fq "if: \${{ needs.candidate.outputs.receipt_found != 'true' }}" "${beta}"
grep -Fq 'core_build: ${{ steps.guard.outputs.core_build }}' "${beta}"
grep -Fq 'updater_build: ${{ steps.guard.outputs.updater_build }}' "${beta}"
grep -Fq 'if: ${{ matrix.build_required == '\''true'\'' }}' "${beta}"
grep -Fq 'Reusing verified unfinished component' "${beta}"
grep -Fq 'Could not determine whether ${TAG} has a digest receipt' "${beta}"
grep -Fq 'already points to ${legacy_digest}; refusing to overwrite it with ${SOURCE_DIGEST}.' "${beta}"
grep -Fq 'name: publish coherent beta aliases' "${beta}"
grep -Fq 'Not moving :beta aliases backwards' "${beta}"
grep -Fq '> ftw-image-digests.json' "${beta}"
grep -Fq 'cmp ftw-image-digests.json existing/ftw-image-digests.json' "${beta}"
grep -Fq '"${source}@${SOURCE_DIGEST}"' "${beta}"
grep -Fq 'COPY scripts/build-core.sh ./scripts/build-core.sh' "${dockerfile}"
grep -Fq 'bash scripts/build-core.sh "$TARGETOS" "$TARGETARCH" /out' "${dockerfile}"
grep -Fq -- '-X main.CandidateTag=${CANDIDATE_TAG:-}' "${core_build}"
grep -Fq 'python3 - "${metadata}" "${GITHUB_SHA}" "${VERSION}"' "${release}"
grep -Fq 'STABLE_COMMIT="$(git rev-list -n 1 "${TAG}")"' "${release}"
grep -Fq '[ "${STABLE_COMMIT}" != "${GITHUB_SHA}" ]' "${release}"
grep -Fq 'source_beta:' "${release}"
grep -Fq 'BETA_TAG="${INPUT_BETA}"' "${release}"
grep -Fq -- '--pattern ftw-image-digests.json' "${release}"
grep -Fq 'current_digest="$(scripts/inspect-image-digest.sh "${image_ref}")"' "${release}"
grep -Fq -- '-f source_beta="${BETA_TAG}"' "${release}"
grep -Fq -- '-f release_id="${RELEASE_ID}"' "${release}"
grep -Fq -- '--json databaseId,tagName,name,body,isDraft,isPrerelease,publishedAt' "${release}"
grep -Fq 'if RELEASE_JSON="$(gh release view "${TAG}"' "${release}"
grep -Fq '.isDraft == true' "${release}"
grep -Fq 'Refreshed draft GitHub Release ${TAG}; release-assets will resume it.' "${release}"
grep -Fq -- '--verify-tag' "${release}"
grep -Fq -- '--draft' "${release}"
grep -Fq 'name: Validate both exact candidate manifests' "${assets}"
grep -Fq 'name: Preflight all exact stable aliases' "${assets}"
grep -Fq 'name: Publish and verify exact stable aliases updater before Core' "${assets}"
grep -Fq '.release-workflow/scripts/promote-paired-latest.sh' "${assets}"
grep -Fq -- '--ref master -f tag=vX.Y.Z -f source_beta=vX.Y.Z-beta.N -f release_id=123' "${assets}"
grep -Fq -- '--ref master -f tag=vX.Y.Z -f release_id=123' "${assets}"
grep -Fq 'name: verify complete draft assets' "${assets}"
grep -Fq 'python3 scripts/check-stable-release.py order "${TAG}"' "${assets}"
grep -Fq 'python3 scripts/check-stable-release.py assets "${TAG}"' "${assets}"
grep -Fq 'name: verify and publish complete stable release' "${assets}"
grep -Fq 'needs: [meta, assets-ready, docker]' "${assets}"
if [ "$(grep -Fc 'GH_TOKEN: ${{ secrets.CI_TOKEN }}' "${assets}")" -ne 6 ]; then
  echo "every draft release read/write must use the repo-scoped release token" >&2
  exit 1
fi
if grep -Fq 'GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}' "${assets}" || \
   grep -Fq 'GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}' "${assets}"; then
  echo "release-assets must not use the per-run token for draft release access" >&2
  exit 1
fi
grep -Fq '"${release_command}" publish "${FTW_RELEASE_ID}"' "${root}/scripts/promote-paired-latest.sh"
grep -Fq -- '-F draft=false' "${root}/scripts/github-release-by-id.sh"
grep -Fq -- '-F prerelease=false' "${root}/scripts/github-release-by-id.sh"
grep -Fq -- '-f make_latest=true' "${root}/scripts/github-release-by-id.sh"
grep -Fq 'needs: [meta, publish]' "${assets}"
grep -Fq 'group: release-assets-stable-latest' "${assets}"
grep -Fq -- '--pattern ftw-image-digests.json' "${assets}"
grep -Fq 'ftw-promotion-receipt.json' "${assets}"
grep -Fq 'source_beta is required for its first promotion.' "${assets}"
grep -Fq 'is already bound to ${BETA_TAG}, not requested ${INPUT_BETA}.' "${assets}"
grep -Fq 'release_id:' "${assets}"
grep -Fq 'scripts/github-release-by-id.sh upload' "${assets}"
grep -Fq 'scripts/github-release-by-id.sh download' "${assets}"
grep -Fq 'current_digest="$(scripts/inspect-image-digest.sh "${source}")"' "${assets}"
grep -Fq 'test "$(scripts/inspect-image-digest.sh "${canonical}:${tag}")" = "${expected}"' "${assets}"
grep -Fq '"${source}@${source_digest}"' "${assets}"
grep -Fq 'sha256sum -c "${checksum_name}"' "${assets}"
grep -Fq 'and ((.imager.devices // []) | length) > 0' "${assets}"
grep -Fq '[ "${STABLE_COMMIT}" != "${GITHUB_SHA}" ]' "${assets}"
grep -Fq '[ "${GITHUB_REF}" != "refs/heads/master" ]' "${assets}"
grep -Fq 'RELEASE_JSON="$(scripts/github-release-by-id.sh show "${RELEASE_ID}")"' "${assets}"
if grep -Fq 'Expected one draft GitHub Release for ${TAG}; found ${RELEASE_COUNT}.' "${assets}"; then
  echo "release-assets must not look for drafts in the REST release collection" >&2
  exit 1
fi
grep -Fq 'FTW_IMAGE_TAG: ${FTW_IMAGE_TAG:-}' "${compose}"
grep -Fq 'FTW_IMAGE_TAG: ${FTW_IMAGE_TAG:-}' "${compose_macos}"

release_checkout="$(grep -n '      - name: Checkout$' "${release}" | cut -d: -f1)"
release_setup_node="$(grep -n '      - name: Setup Node$' "${release}" | cut -d: -f1)"
if ! sed -n "${release_checkout},$((release_setup_node - 1))p" "${release}" | grep -Fq 'persist-credentials: false'; then
  echo "release checkout must not persist a second GitHub auth header" >&2
  exit 1
fi
if [ "$(grep -Fc 'AUTH_HEADER_COUNT="' "${release}")" -ne 2 ]; then
  echo "both release git credential phases must enforce exactly one auth header" >&2
  exit 1
fi

stable_guard="$(grep -n 'STABLE_COMMIT="$(git rev-list -n 1 "${TAG}")"' "${release}" | cut -d: -f1)"
release_lookup="$(grep -n 'if RELEASE_JSON="$(gh release view "${TAG}"' "${release}" | cut -d: -f1)"
if [ "${stable_guard}" -ge "${release_lookup}" ]; then
  echo "existing stable tag guard runs too late" >&2
  exit 1
fi

draft_create="$(grep -n 'gh release create "${TAG}"' "${release}" | cut -d: -f1)"
draft_refresh="$(grep -n 'gh release edit "${TAG}"' "${release}" | cut -d: -f1)"
draft_reuse="$(grep -n 'Refreshed draft GitHub Release' "${release}" | cut -d: -f1)"
draft_verify="$(grep -n 'DRAFT_JSON="$(gh release view' "${release}" | cut -d: -f1)"
dispatch_output="$(grep -n 'echo "published=true"' "${release}" | cut -d: -f1)"
if [ "${draft_create}" -ge "${draft_refresh}" ] || \
   [ "${draft_refresh}" -ge "${draft_reuse}" ] || \
   [ "${draft_reuse}" -ge "${draft_verify}" ] || \
   [ "${draft_verify}" -ge "${dispatch_output}" ]; then
  echo "an existing draft must resume through the asset dispatch output" >&2
  exit 1
fi
draft_create_block="$(sed -n "${draft_create},$((draft_refresh - 1))p" "${release}")"
if ! grep -Fq -- '--verify-tag' <<<"${draft_create_block}" || \
   ! grep -Fq -- '--draft' <<<"${draft_create_block}"; then
  echo "stable release creation must verify the tag and remain draft" >&2
  exit 1
fi
draft_refresh_block="$(sed -n "${draft_refresh},$((draft_reuse - 1))p" "${release}")"
for required in '--draft=true' '--prerelease=false' '--title "${TAG}"' '--notes-file release-notes.md'; do
  if ! grep -Fq -- "${required}" <<<"${draft_refresh_block}"; then
    echo "reused draft refresh is missing ${required}" >&2
    exit 1
  fi
done
draft_verify_block="$(sed -n "${draft_verify},$((dispatch_output - 1))p" "${release}")"
for required in \
  'python3 scripts/check-stable-release.py draft' \
  '"${TAG}" "${STATE_SCHEMA}" release-notes.md'; do
  if ! grep -Fq -- "${required}" <<<"${draft_verify_block}"; then
    echo "refreshed draft verification is missing ${required}" >&2
    exit 1
  fi
done

candidate_guard="$(grep -n '^  candidate:$' "${beta}" | cut -d: -f1)"
beta_docker="$(grep -n '^  docker:$' "${beta}" | cut -d: -f1)"
beta_build="$(grep -n 'uses: docker/build-push-action' "${beta}" | cut -d: -f1)"
if [ "${candidate_guard}" -ge "${beta_docker}" ] || [ "${beta_docker}" -ge "${beta_build}" ]; then
  echo "beta immutability guard must complete before any image build" >&2
  exit 1
fi

beta_release="$(grep -n '^  release:$' "${beta}" | cut -d: -f1)"
beta_channel="$(grep -n '^  channel:$' "${beta}" | cut -d: -f1)"
if [ "${beta_release}" -ge "${beta_channel}" ]; then
  echo "moving beta aliases must wait for the digest receipt" >&2
  exit 1
fi
if sed -n "${beta_build},${beta_release}p" "${beta}" | grep -Fq ':beta'; then
  echo "component builds must not move beta aliases before both images complete" >&2
  exit 1
fi

beta_channel_block="$(sed -n "${beta_channel},\$p" "${beta}")"
channel_line() {
  awk -v needle="$1" 'index($0, needle) { print NR; exit }' <<<"${beta_channel_block}"
}
canonical_login="$(channel_line 'name: Login to canonical GHCR namespace')"
canonical_write="$(channel_line 'name: Move canonical aliases after both exact manifests are recorded')"
compatibility_login="$(channel_line 'name: Login to compatibility GHCR namespace')"
compatibility_write="$(channel_line 'name: Mirror beta aliases to compatibility namespace')"
if [ -z "${canonical_login}" ] || [ -z "${canonical_write}" ] || \
   [ -z "${compatibility_login}" ] || [ -z "${compatibility_write}" ]; then
  echo "beta alias publication must have separate canonical and compatibility phases" >&2
  exit 1
fi
if [ "${canonical_login}" -ge "${canonical_write}" ] || \
   [ "${canonical_write}" -ge "${compatibility_login}" ] || \
   [ "${compatibility_login}" -ge "${compatibility_write}" ]; then
  echo "each beta alias write must run under its namespace credential" >&2
  exit 1
fi
canonical_block="$(sed -n "${canonical_write},$((compatibility_login - 1))p" <<<"${beta_channel_block}")"
if ! grep -Fq -- '--tag "${canonical}:beta"' <<<"${canonical_block}" || \
   grep -Fq -- '--tag "${compatibility}:beta"' <<<"${canonical_block}"; then
  echo "canonical beta aliases must move before the compatibility login" >&2
  exit 1
fi
compatibility_block="$(sed -n "${compatibility_write},\$p" <<<"${beta_channel_block}")"
if ! grep -Fq -- '--tag "${compatibility}:beta"' <<<"${compatibility_block}" || \
   ! grep -Fq '"${canonical_version}@${expected}"' <<<"${compatibility_block}"; then
  echo "compatibility beta aliases must copy the exact canonical manifests" >&2
  exit 1
fi

promotion_upload="$(grep -n 'scripts/github-release-by-id.sh upload' "${assets}" | head -1 | cut -d: -f1)"
stable_docker="$(grep -n '^  docker:$' "${assets}" | cut -d: -f1)"
asset_draft_lookup="$(grep -n 'RELEASE_JSON="$(scripts/github-release-by-id.sh show "${RELEASE_ID}")"' "${assets}" | cut -d: -f1)"
asset_release_inventory="$(grep -n 'RELEASE_PAGES="$(gh api --paginate --slurp' "${assets}" | cut -d: -f1)"
stable_order_guard="$(grep -n 'check-stable-release.py order "${TAG}"' "${assets}" | cut -d: -f1)"
if [ "${asset_draft_lookup}" -ge "${asset_release_inventory}" ] || \
   [ "${asset_release_inventory}" -ge "${stable_order_guard}" ] || \
   [ "${stable_order_guard}" -ge "${stable_docker}" ]; then
  echo "draft lookup and newest-public-stable inventory must run before aliases" >&2
  exit 1
fi
if [ "${promotion_upload}" -ge "${stable_docker}" ]; then
  echo "stable promotion receipt must be bound before alias writes" >&2
  exit 1
fi
if grep -Fq 'git tag --list "${TAG}-beta.*"' "${assets}"; then
  echo "stable asset reruns must not auto-select a newer beta" >&2
  exit 1
fi
if grep -Fq 'git tag --list "${TAG}-beta.*"' "${release}"; then
  echo "stable release must use the beta selected by the operator" >&2
  exit 1
fi

trigger_start="$(grep -n '^on:$' "${assets}" | cut -d: -f1)"
permissions_start="$(grep -n '^permissions:$' "${assets}" | cut -d: -f1)"
if sed -n "${trigger_start},$((permissions_start - 1))p" "${assets}" | grep -q '^  push:'; then
  echo "stable release assets must not run from an unbound tag push" >&2
  exit 1
fi

docker_start="$(grep -n '^  docker:$' "${assets}" | cut -d: -f1)"
discord_start="$(grep -n '^  discord:$' "${assets}" | cut -d: -f1)"
if sed -n "${docker_start},$((discord_start - 1))p" "${assets}" | grep -q 'docker/build-push-action'; then
  echo "stable docker job still rebuilds an image" >&2
  exit 1
fi

stable_docker_block="$(sed -n "${docker_start},$((discord_start - 1))p" "${assets}")"
if grep -qE '^[[:space:]]+(strategy:|matrix:)' <<<"${stable_docker_block}" || \
   grep -Fq '${{ matrix.' <<<"${stable_docker_block}"; then
  echo "Core and updater stable promotion must not run as independent matrix legs" >&2
  exit 1
fi

paired_validation="$(grep -n 'name: Validate both exact candidate manifests' "${assets}" | cut -d: -f1)"
exact_preflight="$(grep -n 'name: Preflight all exact stable aliases' "${assets}" | cut -d: -f1)"
exact_publish="$(grep -n 'name: Publish and verify exact stable aliases updater before Core' "${assets}" | cut -d: -f1)"
stable_publish_job="$(grep -n '^  publish:$' "${assets}" | cut -d: -f1)"
latest_transaction="$(grep -n '.release-workflow/scripts/promote-paired-latest.sh' "${assets}" | cut -d: -f1)"
if [ "${paired_validation}" -ge "${exact_preflight}" ] || \
   [ "${exact_preflight}" -ge "${exact_publish}" ] || \
   [ "${exact_publish}" -ge "${stable_publish_job}" ] || \
   [ "${stable_publish_job}" -ge "${latest_transaction}" ]; then
  echo "both candidates must validate before stable tags or latest aliases move" >&2
  exit 1
fi
validation_block="$(sed -n "${paired_validation},$((exact_preflight - 1))p" "${assets}")"
for required in \
  '"core|ghcr.io/srcfl/ftw"' \
  '"updater|ghcr.io/srcfl/ftw-updater"' \
  'echo "${key}_digest=${source_digest}"'; do
  if ! grep -Fq "${required}" <<<"${validation_block}"; then
    echo "paired validation is missing ${required}" >&2
    exit 1
  fi
done
if grep -Fq 'imagetools create' <<<"${validation_block}"; then
  echo "stable validation moved an alias before both candidates passed" >&2
  exit 1
fi

preflight_block="$(sed -n "${exact_preflight},$((exact_publish - 1))p" "${assets}")"
for required in \
  '"${STABLE_TAG}" "${STABLE_VERSION}" "sha-${short_sha}"' \
  '"updater|${UPDATER_DIGEST}|srcfl/ftw-updater|ghcr.io/srcfl/ftw-updater"' \
  '"core|${CORE_DIGEST}|srcfl/ftw|ghcr.io/srcfl/ftw"' \
  '404)' \
  '200)' \
  'Could not prove ${image}:${tag} absent or exact'; do
  if ! grep -Fq "${required}" <<<"${preflight_block}"; then
    echo "exact stable alias preflight is missing ${required}" >&2
    exit 1
  fi
done
if grep -Fq 'imagetools create' <<<"${preflight_block}"; then
  echo "exact alias preflight mutated a registry tag" >&2
  exit 1
fi

exact_publish_block="$(sed -n "${exact_publish},$((stable_publish_job - 1))p" "${assets}")"
canonical_updater="$(grep -nF 'publish_canonical ghcr.io/srcfl/ftw-updater "${UPDATER_DIGEST}"' <<<"${exact_publish_block}" | cut -d: -f1)"
compatibility_updater="$(grep -nF 'mirror_compatibility ghcr.io/srcfl/ftw-updater \' <<<"${exact_publish_block}" | cut -d: -f1)"
canonical_core="$(grep -nF 'publish_canonical ghcr.io/srcfl/ftw "${CORE_DIGEST}"' <<<"${exact_publish_block}" | cut -d: -f1)"
compatibility_core="$(grep -nF 'mirror_compatibility ghcr.io/srcfl/ftw \' <<<"${exact_publish_block}" | cut -d: -f1)"
if [ -z "${canonical_updater}" ] || [ -z "${compatibility_updater}" ] || \
   [ -z "${canonical_core}" ] || [ -z "${compatibility_core}" ] || \
   [ "${canonical_updater}" -ge "${compatibility_updater}" ] || \
   [ "${compatibility_updater}" -ge "${canonical_core}" ] || \
   [ "${canonical_core}" -ge "${compatibility_core}" ]; then
  echo "all updater stable aliases must write before any Core stable alias" >&2
  exit 1
fi
for required in \
  '"${UPDATER_DIGEST}|ghcr.io/srcfl/ftw-updater|ghcr.io/frahlg/forty-two-watts-updater"' \
  '"${CORE_DIGEST}|ghcr.io/srcfl/ftw|ghcr.io/frahlg/forty-two-watts"' \
  'inspect-image-digest.sh "${canonical}:${tag}"' \
  'inspect-image-digest.sh "${compatibility}:${tag}"'; do
  if ! grep -Fq "${required}" <<<"${exact_publish_block}"; then
    echo "full exact stable alias verification is missing ${required}" >&2
    exit 1
  fi
done

# Immutable version/sha aliases may be left exact or absent on a failed run.
# The moving latest channels must go through the tested rollback helper only.
before_latest="$(sed -n "${exact_publish},$((latest_transaction - 1))p" "${assets}")"
if grep -Eq 'for tag in .*latest' <<<"${before_latest}"; then
  echo "latest moved outside the paired rollback transaction" >&2
  exit 1
fi

asset_gate_job="$(grep -n '^  assets-ready:$' "${assets}" | cut -d: -f1)"
asset_gate_block="$(sed -n "${asset_gate_job},$((docker_start - 1))p" "${assets}")"
for required in \
  'needs: [meta, binaries, imager-metadata]' \
  'python3 scripts/check-stable-release.py assets "${TAG}"' \
  'asset_name="${checksum_name%.sha256}"' \
  '[ "${recorded_name}" != "${asset_name}" ]' \
  'sha256sum -c "${checksum_name}"' \
  'and ((.imager.devices // []) | length) > 0'; do
  if ! grep -Fq -- "${required}" <<<"${asset_gate_block}"; then
    echo "pre-Docker stable asset gate is missing ${required}" >&2
    exit 1
  fi
done
docker_header="$(sed -n "${docker_start},$((paired_validation - 1))p" "${assets}")"
if ! grep -Fq 'needs: [meta, assets-ready]' <<<"${docker_header}"; then
  echo "stable aliases must wait for the verified draft asset gate" >&2
  exit 1
fi

final_publish_block="$(sed -n "${stable_publish_job},$((discord_start - 1))p" "${assets}")"
for required in \
  'needs: [meta, assets-ready, docker]' \
  'packages: write' \
  '.isDraft == true' \
  'for alias in "${TAG}" "${VERSION}" "sha-${short_sha}"; do' \
  '.release-workflow/scripts/promote-paired-latest.sh'; do
  if ! grep -Fq -- "${required}" <<<"${final_publish_block}"; then
    echo "final stable publication gate is missing ${required}" >&2
    exit 1
  fi
done

echo "exact image promotion workflow checks passed"
