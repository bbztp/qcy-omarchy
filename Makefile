PLUGIN_ID  := io.github.bbztp.qcy
BUILD_DIR  := build
BIN_DIR    := $(HOME)/.local/bin
PLUGIN_DIR := $(HOME)/.config/omarchy/plugins/$(PLUGIN_ID)
UNIT_FILE  := $(HOME)/.config/systemd/user/qcyctld.service

.PHONY: build vet install uninstall clean

build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/qcyctl ./cmd/qcyctl
	go build -o $(BUILD_DIR)/qcyctld ./cmd/qcyctld

vet:
	go vet ./...

install: build
	./scripts/install.sh --no-build

uninstall:
	-systemctl --user disable --now qcyctld.service
	rm -f "$(UNIT_FILE)"
	-systemctl --user daemon-reload
	rm -f "$(BIN_DIR)/qcyctl" "$(BIN_DIR)/qcyctld"
	rm -rf "$(PLUGIN_DIR)"
	@command -v omarchy-shell >/dev/null 2>&1 && omarchy-shell -q shell rescanPlugins || true

clean:
	rm -rf $(BUILD_DIR)

.PHONY: protocol
# Regenerate the embedded protocol registry from docs/reference/protocol.json.
protocol:
	python3 scripts/gen_protocol.py
