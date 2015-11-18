#!/bin/bash

docker run --rm -it -v "$GOPATH":/go -w /go/src/github.com/krisrang/cirunner golang sh -c 'GOOS=linux GOARCH=amd64 go build -o cirunner'
