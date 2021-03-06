# This Makefile is used within the release process of the main Datadog Agent to pre-package datadog-trace-agent:
# https://github.com/DataDog/datadog-agent/blob/2b7055c/omnibus/config/software/datadog-trace-agent.rb

# if the TRACE_AGENT_VERSION environment variable isn't set, default to 0.99.0
TRACE_AGENT_VERSION := $(if $(TRACE_AGENT_VERSION),$(TRACE_AGENT_VERSION), 0.99.0)

# break up the version
SPLAT = $(subst ., ,$(TRACE_AGENT_VERSION))
VERSION_MAJOR = $(shell echo $(word 1, $(SPLAT)) | sed 's/[^0-9]*//g')
VERSION_MINOR = $(shell echo $(word 2, $(SPLAT)) | sed 's/[^0-9]*//g')
VERSION_PATCH = $(shell echo $(word 3, $(SPLAT)) | sed 's/[^0-9]*//g')

# account for some defaults
VERSION_MAJOR := $(if $(VERSION_MAJOR),$(VERSION_MAJOR), 0)
VERSION_MINOR := $(if $(VERSION_MINOR),$(VERSION_MINOR), 0)
VERSION_PATCH := $(if $(VERSION_PATCH),$(VERSION_PATCH), 0)

install:
	# generate versioning information and installing the binary.
	go generate ./internal/info
	go install ./cmd/trace-agent

binaries:
	test -n "$(V)" # $$V must be set to the release version tag, e.g. "make binaries V=1.2.3"

	# compiling release binaries for tag $(V)
	git checkout $(V)
	mkdir -p ./bin
	TRACE_AGENT_VERSION=$(V) go generate ./internal/info
	go get -u github.com/karalabe/xgo
	xgo -dest=bin -go=1.10 -out=trace-agent-$(V) -targets=windows-6.1/amd64,linux/amd64,darwin-10.11/amd64 ./cmd/trace-agent
	mv ./bin/trace-agent-$(V)-windows-6.1-amd64.exe ./bin/trace-agent-$(V)-windows-amd64.exe
	mv ./bin/trace-agent-$(V)-darwin-10.11-amd64 ./bin/trace-agent-$(V)-darwin-amd64 
	git reset --hard head && git checkout -

ci:
	# task used by CI
	go get -u golang.org/x/lint/golint
	golint -set_exit_status=1 ./cmd/trace-agent ./internal/filters ./internal/api ./internal/test ./internal/info ./internal/quantile ./internal/obfuscate ./internal/sampler ./internal/metrics ./internal/watchdog ./internal/writer ./internal/flags ./internal/osutil
	go install ./cmd/trace-agent
	go test -v -race ./...

windows:
	# pre-packages resources needed for the windows release
	windmc --target pe-x86-64 -r cmd/trace-agent/windows_resources cmd/trace-agent/windows_resources/trace-agent-msg.mc
	windres --define MAJ_VER=$(VERSION_MAJOR) --define MIN_VER=$(VERSION_MINOR) --define PATCH_VER=$(VERSION_PATCH) -i cmd/trace-agent/windows_resources/trace-agent.rc --target=pe-x86-64 -O coff -o cmd/trace-agent/rsrc.syso
