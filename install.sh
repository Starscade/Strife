#!/bin/sh

GIT_BRANCH=$(git branch --show-current)
GIT_TAG=$(git describe --tags)
INSTALL_DIR=~/.local/bin
LDFLAGS="-s -w -X 'main.version=${GIT_TAG}-${GIT_BRANCH}'"

mkdir -p "$INSTALL_DIR" &&
go mod tidy &&
go fmt . &&
CGO_ENABLED=0 &&
go build \
	-ldflags="$LDFLAGS" \
	-v \
	-x \
	-o "${INSTALL_DIR}/Strife" .
