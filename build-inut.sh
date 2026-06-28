#!/usr/bin/env bash
set -e

GO=/home/ksp/inut-rk3528-browswer/wpebuild/godl/go/bin/go
BUILDTIME=$(date +%d%m%Y_%H%M%S)
LDFLAGS="-X main.inutBuildTime=${BUILDTIME}"

echo "Building go2rtc_test (linux/amd64) build=${BUILDTIME}..."
$GO build -ldflags "$LDFLAGS" -o go2rtc_test .

echo "Done: go2rtc_test"
./go2rtc_test -v
