PLUGIN_NAME := deepseek-vision
PLUGIN_VERSION := 0.1.1

.PHONY: test race vet build verify-version-override clean

test:
	GOTOOLCHAIN=auto go test ./...

race:
	GOTOOLCHAIN=auto go test -race ./...

vet:
	GOTOOLCHAIN=auto go vet ./...

build:
	CGO_ENABLED=1 GOTOOLCHAIN=auto go build -buildmode=c-shared -trimpath -ldflags="-s -w -X main.pluginVersion=$(PLUGIN_VERSION)" -o $(PLUGIN_NAME).so .

# Confirms the release linker flag reaches registration's pluginVersion data.
verify-version-override:
	@artifact="$$(mktemp /tmp/deepseek-vision-version-XXXXXX.so)"; \
	trap 'rm -f "$$artifact" "$${artifact%.so}.h"' EXIT; \
	CGO_ENABLED=1 GOTOOLCHAIN=auto go build -buildmode=c-shared -trimpath -ldflags="-X main.pluginVersion=$(PLUGIN_VERSION)-link-check" -o "$$artifact" .; \
	strings "$$artifact" | grep -F "$(PLUGIN_VERSION)-link-check" >/dev/null

clean:
	rm -f $(PLUGIN_NAME).so $(PLUGIN_NAME).h
