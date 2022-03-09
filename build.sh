#!/bin/bash
set -e 

version=0.0.1

rm -rf ./builds/*

echo "Building are-we-there-yet"
CGO_ENABLED=0 go build -ldflags="-X 'main.version=${version}'" -o builds/are-we-there-yet .
chmod 555 builds/are-we-there-yet