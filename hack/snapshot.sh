#!/usr/bin/env bash
set -euo pipefail

SNAPSHOT_TAG=$(git rev-parse HEAD)
RELEASE_REPO=${RELEASE_REPO:-public.ecr.aws/karpenter/}

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
source "${SCRIPT_DIR}/release_common.sh"

publishHelmChart() {
  (
    HELM_CHART_FILE_NAME="karpenter-${HELM_CHART_VERSION}.tgz"

    cd charts
    helm lint karpenter
    helm package karpenter --version $HELM_CHART_VERSION
    helm push "${HELM_CHART_FILE_NAME}" "oci://${RELEASE_REPO}"
    rm "${HELM_CHART_FILE_NAME}"
  )
}

notifyIfStableRelease() {
  COMMIT_TAG=$(git describe --tags --exact-match || echo "none")
  if [[ "${COMMIT_TAG}" == "none" || "${COMMIT_TAG}" != v* ]]; then
    echo "Not sending a stable release message since no valid stable tag releases found in tags of this commit: '${COMMIT_TAG}'"
    return
  fi
  notifyRelease "stable" $COMMIT_TAG
}

authenticate
buildImages $HELM_CHART_VERSION
cosignImages
publishHelmChart
notifyIfStableRelease
pullPrivateReplica "snapshot" $SNAPSHOT_TAG
notifyRelease "snapshot" $HELM_CHART_VERSION
