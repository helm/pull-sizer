#!/usr/bin/env bash

# Copyright The Helm Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
set -euo pipefail

VERSION=
if [[ -n "${CIRCLE_TAG:-}" ]]; then
  VERSION="${CIRCLE_TAG}"
elif [[ "${CIRCLE_BRANCH:-}" == "master" ]]; then
  VERSION="latest"
else
  echo "Skipping deploy step; this is neither master or a tag"
  exit
fi

echo "Building binary"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/pull-sizer *.go

echo "Building docker image"
docker build -t quay.io/helmpack/pull-sizer:$VERSION .

echo "Logging in to Quay"
docker login -u $DOCKER_USER -p $DOCKER_PASS quay.io

echo "Pushing image"
docker push quay.io/helmpack/pull-sizer:$VERSION