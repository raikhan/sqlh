set windows-shell := ["bash", "-cu"]

bin := if os_family() == "windows" { "sqlh.exe" } else { "sqlh" }
revision := `git rev-parse --short HEAD`
ldflags := "-s -w -X main.revision=" + revision
goos := `go env GOOS`
gopath_bin := `go env GOPATH` / "bin"
# `go install` puts the binary in $GOBIN, or $GOPATH/bin when GOBIN is unset
install_dir := env("GOBIN", gopath_bin)
gobump := gopath_bin / "gobump"

export GO111MODULE := "on"

default: clean build

build:
    go build -ldflags="{{ldflags}}" -o {{bin}} .

release: _gobump build
    zip -r sqlh-{{goos}}-$({{gobump}} show -r .).zip {{bin}}

# install into $GOBIN / $GOPATH/bin (the editor starts `sqlh` from PATH)
install:
    go install -ldflags="{{ldflags}}" .
    @echo "installed {{install_dir / bin}}"
    @if [ "$(command -v {{bin}})" != "{{install_dir / bin}}" ]; then \
        echo "warning: '{{bin}}' on PATH resolves to '$(command -v {{bin}})', not {{install_dir / bin}}"; \
    fi

show-version: _gobump
    @{{gobump}} show -r .

test: build
    go test -v ./...

clean:
    go clean

bump: _gobump
    #!/usr/bin/env bash
    set -euo pipefail
    if [ -n "$(git status --porcelain)" ]; then echo "git workspace is dirty" >&2; exit 1; fi
    if [ "$(git rev-parse --abbrev-ref HEAD)" != "master" ]; then echo "current branch is not master" >&2; exit 1; fi
    {{gobump}} up -w .
    version="$({{gobump}} show -r .)"
    git commit -am "bump up version to $version"
    git tag "v$version"
    git push origin master
    git push origin "refs/tags/v$version"

_gobump:
    @[ -x "{{gobump}}" ] || env GOOS=$(go env GOHOSTOS) GOARCH=$(go env GOHOSTARCH) go install github.com/x-motemen/gobump/cmd/gobump@latest
