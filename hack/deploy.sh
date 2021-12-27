#!/usr/bin/env bash
set -o errexit
set -o nounset
set -o pipefail

export VERSION=$(git describe --tags --always --dirty="-dev")
echo "$DOCKER_PASSWORD" | docker login -u "$DOCKER_USERNAME" --password-stdin
IMAGE=form3tech/etcd-operator:$VERSION ./hack/build/docker_push
