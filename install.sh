#!/bin/sh

INSTALL_DIR=~/.local/bin

mkdir -p "$INSTALL_DIR" &&
go mod tidy &&
go fmt . &&
CGO_ENABLED=0 go build -ldflags="-s -w" -v -x -o "${INSTALL_DIR}/Strife" .
